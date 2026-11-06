package androiddev

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

func challengeDevice() *Device { return New(&fakeTransport{}, Config{Serial: "emulator-test"}) }

func get(t *testing.T, c *http.Client, u string) (string, error) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pinnedClient trusts nothing but a leaf whose DER digest equals pin.
func pinnedClient(pin string) *http.Client {
	verify := func(raw [][]byte, _ [][]*x509.Certificate) error {
		sum := sha256.Sum256(raw[0])
		if hex.EncodeToString(sum[:]) != pin {
			return errors.New("pin mismatch")
		}
		return nil
	}
	return &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, VerifyPeerCertificate: verify}}} //nolint:gosec -- pin-only client fixture
}

func TestTLSChallengeRejectsNormalClientAndReceiptsUnsafeCompletion(t *testing.T) {
	c, err := challengeDevice().StartTLSChallenge(context.Background(),
		envspec.TLSChallengeOptions{ExpectedHost: "127.0.0.1", AdvertiseHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := c.Receipt()
	if r.Failure != envspec.TLSFailureUntrustedChain || len(r.CertificateSHA256) != 64 ||
		len(r.CertificateDER) == 0 || r.ExpectedHost != "127.0.0.1" || r.URL != c.URL() {
		t.Fatalf("receipt = %+v", r)
	}
	if sum := sha256.Sum256(r.CertificateDER); hex.EncodeToString(sum[:]) != r.CertificateSHA256 {
		t.Fatal("certificate digest does not match DER")
	}

	if _, err := get(t, &http.Client{Timeout: 3 * time.Second}, c.URL()); err == nil {
		t.Fatal("system-trust client accepted the untrusted challenge")
	}
	if _, err := get(t, pinnedClient(strings.Repeat("0", 64)), c.URL()); err == nil {
		t.Fatal("client pinned to another certificate accepted the challenge")
	}
	if s := c.Receipt().Sessions; len(s) != 0 {
		t.Fatalf("rejected connections counted as completed: %+v", s)
	}

	unsafe := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec -- deliberate vulnerable-client fixture
	if body, err := get(t, unsafe, c.URL()); err != nil || body != "craggy tls challenge\n" {
		t.Fatalf("unsafe body/err = %q/%v", body, err)
	}
	if body, err := get(t, pinnedClient(r.CertificateSHA256), c.URL()); err != nil || body == "" {
		t.Fatalf("pinned to the challenge certificate: body/err = %q/%v", body, err)
	}
	waitFor(t, "two sessions", func() bool { return len(c.Receipt().Sessions) == 2 })
	for _, s := range c.Receipt().Sessions {
		if s.Version < tls.VersionTLS12 || s.RemoteAddr == "" || s.At == "" {
			t.Fatalf("session = %+v", s)
		}
	}
}

func TestTLSChallengeDefaultsToEmulatorAliasOnLoopback(t *testing.T) {
	c, err := challengeDevice().StartTLSChallenge(context.Background(), envspec.TLSChallengeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	u, err := url.Parse(c.URL())
	if err != nil || u.Scheme != "https" || u.Hostname() != envspec.ChallengeHostAlias {
		t.Fatalf("url = %q err=%v", c.URL(), err)
	}
	r := c.Receipt()
	cert, err := x509.ParseCertificate(r.CertificateDER)
	if err != nil || r.ExpectedHost != envspec.ChallengeHostAlias || cert.VerifyHostname(envspec.ChallengeHostAlias) != nil {
		t.Fatalf("expected host %q / cert err %v", r.ExpectedHost, err)
	}
	// The emulator alias is the host loopback: the listener must be
	// reachable there (and only there).
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", u.Port()), time.Second)
	if err != nil {
		t.Fatalf("loopback dial: %v", err)
	}
	conn.Close()
}

func TestUntrustedServerCertificateBindsExpectedHost(t *testing.T) {
	_, der, err := untrustedServerCertificate("api.example.com", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("api.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("other.example.com"); err == nil {
		t.Fatal("certificate unexpectedly matches another host")
	}
	if err := requireUntrustedChain(der, "api.example.com"); err != nil {
		t.Fatalf("validation failure is not untrusted-chain: %v", err)
	}
}

func TestCleartextChallengeReceiptsOnlyExactPath(t *testing.T) {
	c, err := challengeDevice().StartCleartextChallenge(context.Background(),
		envspec.CleartextChallengeOptions{AdvertiseHost: "127.0.0.1", Response: []byte("ok")})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.HasPrefix(c.URL(), "http://127.0.0.1:") || !strings.Contains(c.URL(), "/craggy/cleartext/") {
		t.Fatalf("url = %q", c.URL())
	}
	client := &http.Client{Timeout: 3 * time.Second}
	if body, err := get(t, client, c.URL()); err != nil || body != "ok" {
		t.Fatalf("body/err = %q/%v", body, err)
	}
	wrong := c.URL()[:strings.LastIndex(c.URL(), "/")+1] + "wrong"
	if _, err := get(t, client, wrong); err != nil {
		t.Fatal(err)
	}
	r := c.Receipt()
	if len(r.Requests) != 2 || !r.Requests[0].Matched || r.Requests[1].Matched || r.Requests[0].Method != http.MethodGet {
		t.Fatalf("requests = %+v", r.Requests)
	}
	path := c.URL()[strings.Index(c.URL(), "/craggy/"):]
	sum := sha256.Sum256([]byte(path))
	if r.Requests[0].PathSHA256 != r.PathSHA256 || r.PathSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("path digest mismatch: %+v", r)
	}
	if strings.Contains(r.Requests[0].PathSHA256, path[len("/craggy/cleartext/"):]) {
		t.Fatalf("receipt leaked the path: %+v", r.Requests[0])
	}
}

// The device owns its challenges: detach (lease release/expiry/reset)
// closes them, the receipt survives, and Close is idempotent.
func TestChallengesClosedByDetachAndContext(t *testing.T) {
	d := challengeDevice()
	opts := envspec.CleartextChallengeOptions{AdvertiseHost: "127.0.0.1"}
	owned, err := d.StartCleartextChallenge(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	scoped, err := d.StartCleartextChallenge(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	if _, err := get(t, client, owned.URL()); err != nil {
		t.Fatal(err)
	}

	cancel()
	waitFor(t, "ctx-scoped challenge to close", func() bool {
		_, err := get(t, client, scoped.URL())
		return err != nil
	})
	waitFor(t, "ctx-scoped challenge to untrack", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.challenges) == 1
	})

	if err := d.DetachCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, client, owned.URL()); err == nil {
		t.Fatal("challenge still serving after detach")
	}
	if n := len(owned.Receipt().Requests); n != 1 {
		t.Fatalf("receipt after close has %d requests, want 1", n)
	}
	if err := owned.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	d.mu.Lock()
	left := len(d.challenges)
	d.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d challenges still tracked", left)
	}
}
