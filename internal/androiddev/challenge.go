package androiddev

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// Challenge is a host-owned peer the device is pointed at. Its receipt is
// observed by the host, independently of whatever the app reports. The
// owning Device closes it on detach or destroy; ctx cancellation passed
// to the starter and Close end it earlier.
type Challenge[R any] struct {
	url     string
	receipt func() R
	srv     *http.Server
	done    chan struct{}
	once    sync.Once
	err     error
	untrack func()
}

// URL is the address handed to the app under test.
func (c *Challenge[R]) URL() string { return c.url }

// Receipt snapshots the challenge and everything it observed so far.
func (c *Challenge[R]) Receipt() R { return c.receipt() }

// Close stops the server and waits for it; later calls return the first
// result.
func (c *Challenge[R]) Close() error {
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.err = c.srv.Shutdown(ctx)
		select {
		case <-c.done:
		case <-ctx.Done():
			if c.err == nil {
				c.err = ctx.Err()
			}
		}
		c.untrack()
	})
	return c.err
}

// observed is an append-only, concurrency-safe receipt log.
type observed[T any] struct {
	mu    sync.Mutex
	items []T
}

func (o *observed[T]) add(v T) {
	o.mu.Lock()
	o.items = append(o.items, v)
	o.mu.Unlock()
}

func (o *observed[T]) snapshot() []T {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]T(nil), o.items...)
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// listenChallenge binds a listener the device can reach through
// advertise (default envspec.ChallengeHostAlias). The emulator alias maps
// to the host loopback, so loopback advertise hosts bind loopback only;
// any other bridge binds every interface.
func listenChallenge(advertise string) (net.Listener, string, error) {
	if advertise == "" {
		advertise = envspec.ChallengeHostAlias
	}
	addr := "0.0.0.0:0"
	if ip := net.ParseIP(advertise); advertise == envspec.ChallengeHostAlias || (ip != nil && ip.IsLoopback()) {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("android: challenge listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, net.JoinHostPort(advertise, strconv.Itoa(port)), nil
}

// serveChallenge serves srv on ln and registers the challenge with the
// device so detach/destroy closes it.
func serveChallenge[R any](ctx context.Context, d *Device, ln net.Listener, srv *http.Server, url string, receipt func() R) *Challenge[R] {
	srv.ErrorLog = log.New(io.Discard, "", 0) // rejected handshakes are the expected outcome, not errors
	c := &Challenge[R]{url: url, receipt: receipt, srv: srv, done: make(chan struct{})}
	c.untrack = d.trackChallenge(c)
	go func() {
		defer close(c.done)
		_ = srv.Serve(ln)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.done:
		}
	}()
	return c
}

func (d *Device) trackChallenge(c io.Closer) func() {
	d.mu.Lock()
	if d.challenges == nil {
		d.challenges = map[io.Closer]struct{}{}
	}
	d.challenges[c] = struct{}{}
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		delete(d.challenges, c)
		d.mu.Unlock()
	}
}

// CloseChallenges closes every challenge this device still owns.
func (d *Device) CloseChallenges() error {
	d.mu.Lock()
	owned := make([]io.Closer, 0, len(d.challenges))
	for c := range d.challenges {
		owned = append(owned, c)
	}
	d.mu.Unlock()
	var errs []error
	for _, c := range owned {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// StartTLSChallenge starts a TLS peer whose leaf name matches
// ExpectedHost but whose CA is ephemeral and untrusted, so the only
// validation failure a correct client sees is untrusted-chain. Sessions
// that complete a handshake are receipted.
func (d *Device) StartTLSChallenge(ctx context.Context, opts envspec.TLSChallengeOptions) (*Challenge[envspec.TLSChallengeReceipt], error) {
	advertise := opts.AdvertiseHost
	if advertise == "" {
		advertise = envspec.ChallengeHostAlias
	}
	host := opts.ExpectedHost
	if host == "" {
		host = advertise
	}
	cert, leafDER, err := untrustedServerCertificate(host, time.Now().UTC())
	if err != nil {
		return nil, fmt.Errorf("android: TLS challenge certificate: %w", err)
	}
	if err := requireUntrustedChain(leafDER, host); err != nil {
		return nil, err
	}
	ln, hostPort, err := listenChallenge(advertise)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(leafDER)
	base := envspec.TLSChallengeReceipt{URL: "https://" + hostPort + "/", ExpectedHost: host,
		Failure: envspec.TLSFailureUntrustedChain, CertificateSHA256: hex.EncodeToString(sum[:]),
		CertificateDER: leafDER, StartedAt: nowStamp()}
	response := opts.Response
	if response == nil {
		response = []byte("craggy tls challenge\n")
	}
	var sessions observed[envspec.TLSSession]
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(response)
		}),
		ConnState: func(conn net.Conn, state http.ConnState) {
			tc, ok := conn.(*tls.Conn)
			if state != http.StateActive || !ok {
				return
			}
			cs := tc.ConnectionState()
			sessions.add(envspec.TLSSession{At: nowStamp(), RemoteAddr: conn.RemoteAddr().String(),
				Version: cs.Version, CipherSuite: cs.CipherSuite})
		}}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	return serveChallenge(ctx, d, tlsLn, srv, base.URL, func() envspec.TLSChallengeReceipt {
		r := base
		r.Sessions = sessions.snapshot()
		return r
	}), nil
}

// requireUntrustedChain proves the leaf fails validation for exactly one
// reason: an unknown authority (the name itself matches).
func requireUntrustedChain(leafDER []byte, host string) error {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return err
	}
	_, verr := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: x509.NewCertPool()})
	var unknown x509.UnknownAuthorityError
	switch {
	case verr == nil:
		return fmt.Errorf("android: TLS challenge certificate unexpectedly trusted")
	case !errors.As(verr, &unknown):
		return fmt.Errorf("android: TLS challenge has wrong validation failure: %w", verr)
	}
	return nil
}

// StartCleartextChallenge starts an HTTP-only peer carrying an
// unguessable path. Every request is receipted by path digest; only the
// exact path is served.
func (d *Device) StartCleartextChallenge(ctx context.Context, opts envspec.CleartextChallengeOptions) (*Challenge[envspec.CleartextChallengeReceipt], error) {
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("android: cleartext challenge token: %w", err)
	}
	path := "/craggy/cleartext/" + hex.EncodeToString(secret)
	want := sha256.Sum256([]byte(path))
	ln, hostPort, err := listenChallenge(opts.AdvertiseHost)
	if err != nil {
		return nil, err
	}
	base := envspec.CleartextChallengeReceipt{URL: "http://" + hostPort + path,
		PathSHA256: hex.EncodeToString(want[:]), StartedAt: nowStamp()}
	response := opts.Response
	if response == nil {
		response = []byte("craggy cleartext challenge\n")
	}
	var requests observed[envspec.CleartextRequest]
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sum := sha256.Sum256([]byte(r.URL.EscapedPath()))
			matched := subtle.ConstantTimeCompare(sum[:], want[:]) == 1
			requests.add(envspec.CleartextRequest{At: nowStamp(), RemoteAddr: r.RemoteAddr,
				Method: r.Method, PathSHA256: hex.EncodeToString(sum[:]), Matched: matched})
			if !matched {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write(response)
		})}
	return serveChallenge(ctx, d, ln, srv, base.URL, func() envspec.CleartextChallengeReceipt {
		r := base
		r.Requests = requests.snapshot()
		return r
	}), nil
}

// untrustedServerCertificate issues a leaf for host from a fresh
// ephemeral CA. The returned chain carries the CA so a client that skips
// verification still sees the full presented path.
func untrustedServerCertificate(host string, now time.Time) (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caTemplate := &x509.Certificate{Subject: pkix.Name{CommonName: "Craggy ephemeral challenge CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	if caTemplate.SerialNumber, err = randomSerial(); err != nil {
		return tls.Certificate{}, nil, err
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafTemplate := &x509.Certificate{Subject: pkix.Name{CommonName: host},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if leafTemplate.SerialNumber, err = randomSerial(); err != nil {
		return tls.Certificate{}, nil, err
	}
	if ip := net.ParseIP(host); ip != nil {
		leafTemplate.IPAddresses = []net.IP{ip}
	} else {
		leafTemplate.DNSNames = []string{host}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	return cert, leafDER, err
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
}
