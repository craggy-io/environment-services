// Package adb is a small client for the adb server protocol (port 5037),
// so the driver never spawns adb.exe per call. It
// implements host queries, host:transport, shell protocol v2 (separate
// stdout/stderr plus exit code), exec: for binary-safe streams, and sync:
// for single files.
package adb

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Device is one row of host:devices.
type Device struct {
	Serial string
	State  string // device, offline, unauthorized
}

// Client talks to one adb server.
type Client struct {
	Addr    string // default 127.0.0.1:5037
	Timeout time.Duration
}

// New builds a client for the local adb server.
func New() *Client { return &Client{Addr: "127.0.0.1:5037", Timeout: 15 * time.Second} }

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	addr := c.Addr
	if addr == "" {
		addr = "127.0.0.1:5037"
	}
	d := net.Dialer{Timeout: c.Timeout}
	return d.DialContext(ctx, "tcp", addr)
}

// request sends a length-prefixed service string and checks OKAY/FAIL.
func request(conn net.Conn, service string) error {
	if _, err := fmt.Fprintf(conn, "%04x%s", len(service), service); err != nil {
		return err
	}
	return readStatus(conn, service)
}

func readStatus(conn net.Conn, service string) error {
	var status [4]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return err
	}
	switch string(status[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, _ := readLengthPrefixed(conn)
		return fmt.Errorf("adb: %s failed: %s", service, msg)
	default:
		return fmt.Errorf("adb: unexpected status %q for %s", status, service)
	}
}

func readLengthPrefixed(conn net.Conn) (string, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return "", err
	}
	var n int
	if _, err := fmt.Sscanf(string(lenBuf[:]), "%04x", &n); err != nil {
		return "", fmt.Errorf("adb: bad length %q", lenBuf)
	}
	buf := make([]byte, n)
	_, err := io.ReadFull(conn, buf)
	return string(buf), err
}

// Version returns the server version (host:version).
func (c *Client) Version(ctx context.Context) (string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := request(conn, "host:version"); err != nil {
		return "", err
	}
	return readLengthPrefixed(conn)
}

// Devices lists attached devices/emulators.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := request(conn, "host:devices"); err != nil {
		return nil, err
	}
	body, err := readLengthPrefixed(conn)
	if err != nil {
		return nil, err
	}
	var out []Device
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if line == "" {
			continue
		}
		serial, state, _ := strings.Cut(line, "\t")
		out = append(out, Device{Serial: strings.TrimSpace(serial), State: strings.TrimSpace(state)})
	}
	return out, nil
}

// KillServer asks the adb server to exit (host:kill). Used by the adb
// component repair when every device is offline.
func (c *Client) KillServer(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	// host:kill closes without an OKAY.
	_, err = fmt.Fprintf(conn, "%04xhost:kill", len("host:kill"))
	return err
}

func (c *Client) transport(ctx context.Context, serial string) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := request(conn, "host:transport:"+serial); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Shell Streams: stdout/stderr demuxed from shell protocol v2 frames.
// Frame: 1 id byte + uint32le length + payload. IDs: 0 stdin (client→
// device), 1 stdout, 2 stderr, 3 exit (1-byte code), 4 close-stdin.
const (
	frameStdin    = 0
	frameStdout   = 1
	frameStderr   = 2
	frameExit     = 3
	frameCloseIn  = 4
	maxFrameChunk = 64 << 10
)

// ShellProc is one shell v2 session.
type ShellProc struct {
	Stdout io.Reader
	Stderr io.Reader
	Stdin  io.WriteCloser

	conn net.Conn
	done chan struct{}
	exit int
	err  error
	once sync.Once
}

// Wait blocks for the exit frame and returns the exit code.
func (p *ShellProc) Wait() (int, error) {
	<-p.done
	return p.exit, p.err
}

// Kill closes the connection (the daemon kills the process group).
func (p *ShellProc) Kill() error { return p.conn.Close() }

type frameWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *frameWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFrameChunk {
			chunk = chunk[:maxFrameChunk]
		}
		hdr := make([]byte, 5)
		hdr[0] = frameStdin
		binary.LittleEndian.PutUint32(hdr[1:], uint32(len(chunk)))
		if _, err := w.conn.Write(hdr); err != nil {
			return total, err
		}
		if _, err := w.conn.Write(chunk); err != nil {
			return total, err
		}
		total += len(chunk)
		p = p[len(chunk):]
	}
	return total, nil
}

func (w *frameWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	hdr := []byte{frameCloseIn, 0, 0, 0, 0}
	_, err := w.conn.Write(hdr)
	return err
}

// Shell opens a shell v2 session. cmd is passed as ONE string to the
// device shell; quote arguments with Quote when building from argv.
func (c *Client) Shell(ctx context.Context, serial, cmd string) (*ShellProc, error) {
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return nil, err
	}
	if err := request(conn, "shell,v2,TERM=dumb:"+cmd); err != nil {
		conn.Close()
		return nil, err
	}
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	p := &ShellProc{Stdout: outR, Stderr: errR, Stdin: &frameWriter{conn: conn},
		conn: conn, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer outW.Close()
		defer errW.Close()
		br := bufio.NewReader(conn)
		hdr := make([]byte, 5)
		for {
			if _, err := io.ReadFull(br, hdr); err != nil {
				if err != io.EOF && err != io.ErrUnexpectedEOF {
					p.err = err
				}
				return
			}
			id := hdr[0]
			n := binary.LittleEndian.Uint32(hdr[1:5])
			switch id {
			case frameStdout:
				if _, err := io.CopyN(outW, br, int64(n)); err != nil {
					p.err = err
					return
				}
			case frameStderr:
				if _, err := io.CopyN(errW, br, int64(n)); err != nil {
					p.err = err
					return
				}
			case frameExit:
				b := make([]byte, n)
				if _, err := io.ReadFull(br, b); err == nil && len(b) > 0 {
					p.exit = int(b[0])
				}
				return
			default:
				if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	return p, nil
}

// ShellLegacy uses the v1 shell (merged stream, no exit code). Some old
// devices lack v2; the driver prefers Shell and falls back here.
func (c *Client) ShellLegacy(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() (int, error), error) {
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return nil, nil, err
	}
	if err := request(conn, "shell:"+cmd); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(conn, stdin)
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
	}
	done := make(chan struct{})
	go func() { <-ctx.Done(); conn.Close() }()
	wait := func() (int, error) {
		// Reading to EOF is completion; exit code unknown in v1.
		return 0, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{conn, connCloser{conn, done}}, wait, nil
}

type connCloser struct {
	net.Conn
	done chan struct{}
}

func (c connCloser) Close() error { return c.Conn.Close() }

// ExecOut opens an exec: raw stream (binary-safe; stderr merged to the
// log). Used for screencap and tar.
func (c *Client) ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error) {
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return nil, nil, err
	}
	if err := request(conn, "exec:"+cmd); err != nil {
		conn.Close()
		return nil, nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(conn, stdin)
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
	}
	go func() { <-ctx.Done(); conn.Close() }()
	return conn, func() error { return conn.Close() }, nil
}

// Forward sets up a host→device TCP forward (host-serial:…:forward).
// A zero local port asks adb to allocate an unused port atomically. A zero
// remote port removes the named forward. The result is the local port.
func (c *Client) Forward(ctx context.Context, serial string, localPort, remotePort int) (int, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	deadline := time.Now().Add(c.Timeout)
	if c.Timeout <= 0 {
		deadline = time.Now().Add(15 * time.Second)
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	local := fmt.Sprintf("tcp:%d", localPort)
	if remotePort == 0 {
		if err := request(conn, fmt.Sprintf("host-serial:%s:killforward:%s", serial, local)); err != nil {
			return 0, err
		}
		return localPort, readStatus(conn, "killforward")
	}
	if err := request(conn, fmt.Sprintf("host-serial:%s:forward:%s;tcp:%d", serial, local, remotePort)); err != nil {
		return 0, err
	}
	// The host service acknowledges selection, then the forward operation.
	if err := readStatus(conn, "forward"); err != nil {
		return 0, err
	}
	if localPort != 0 {
		return localPort, nil
	}
	assigned, err := readLengthPrefixed(conn)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(assigned)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("adb: invalid allocated port %q", assigned)
	}
	return port, nil
}

// Root restarts adbd as root and waits for the reply text.
func (c *Client) Root(ctx context.Context, serial string) (string, error) {
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := request(conn, "root:"); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	b, _ := io.ReadAll(conn)
	return string(b), nil
}

// Emu sends an emulator console command (snapshots, kill). It speaks to
// the emulator console port directly (serial emulator-<port>), handling
// the auth-token handshake from ~/.emulator_console_auth_token, and
// falls back to the adb emu: service when the port is unknown.
// KOError is an emulator console "KO: ..." rejection — a definitive
// answer, never something to paper over with a fallback.
type KOError struct{ Message string }

func (e *KOError) Error() string { return "adb: emulator console: " + e.Message }

func (c *Client) Emu(ctx context.Context, serial, cmd string) (string, error) {
	if strings.HasPrefix(serial, "emulator-") {
		out, err := c.emuConsole(ctx, serial, cmd)
		if err == nil {
			return out, nil
		}
		var ko *KOError
		if errors.As(err, &ko) {
			return out, err
		}
	}
	conn, err := c.transport(ctx, serial)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := request(conn, "emu:"+cmd); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	b, _ := io.ReadAll(conn)
	return string(b), nil
}

// emuConsole runs one console command over TCP with the auth handshake.
func (c *Client) emuConsole(ctx context.Context, serial, cmd string) (string, error) {
	port := strings.TrimPrefix(serial, "emulator-")
	d := net.Dialer{Timeout: 10 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+port)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(180 * time.Second))
	br := bufio.NewReader(conn)
	// The console greeting itself ends with an "OK" line; drain it first,
	// then authenticate and drain the auth reply's OK. (Consuming only one
	// OK leaves the auth reply buffered and every command reads it as an
	// empty success.)
	readUntilOK := func() error {
		for i := 0; i < 16; i++ {
			line, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "OK") {
				return nil
			}
			if strings.HasPrefix(trimmed, "KO") {
				return &KOError{Message: trimmed}
			}
		}
		return nil
	}
	if token := consoleAuthToken(); token != "" {
		if err := readUntilOK(); err != nil {
			return "", err
		}
		if _, err := fmt.Fprintf(conn, "auth %s\r\n", token); err != nil {
			return "", err
		}
		if err := readUntilOK(); err != nil {
			return "", err
		}
	} else {
		// No token file: drain the greeting best-effort.
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = br.ReadString('\n')
		_ = conn.SetReadDeadline(time.Now().Add(180 * time.Second))
	}
	if _, err := fmt.Fprintf(conn, "%s\r\n", cmd); err != nil {
		return "", err
	}
	var out strings.Builder
	for i := 0; i < 2048; i++ {
		line, err := br.ReadString('\n')
		out.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "OK" {
			return out.String(), nil
		}
		if strings.HasPrefix(trimmed, "KO") {
			return out.String(), &KOError{Message: trimmed}
		}
		if err != nil {
			if out.Len() > 0 {
				return out.String(), nil
			}
			return "", err
		}
	}
	return out.String(), nil
}

// consoleAuthToken reads the emulator console auth token.
func consoleAuthToken() string {
	if v := os.Getenv("ANDROID_EMULATOR_CONSOLE_AUTH_TOKEN"); v != "" {
		return v
	}
	b, err := os.ReadFile(filepath.Join(homeDir(), ".emulator_console_auth_token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("USERPROFILE")
}

// Run executes a shell command to completion and returns merged stdout.
func (c *Client) Run(ctx context.Context, serial, cmd string) (string, int, error) {
	p, err := c.Shell(ctx, serial, cmd)
	if err != nil {
		return "", -1, err
	}
	var outBuf, errBuf strings.Builder
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&outBuf, p.Stdout); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&errBuf, p.Stderr); done <- struct{}{} }()
	code, werr := p.Wait()
	<-done
	<-done
	out := outBuf.String()
	if errBuf.Len() > 0 {
		// Callers parsing output usually only want stdout; stderr kept
		// separate in Shell; Run appends it only on failure.
		if code != 0 {
			out += errBuf.String()
		}
	}
	return out, code, werr
}

// Quote joins argv into one device-shell-safe string.
func Quote(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellQuote(a)
	}
	return strings.Join(parts, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '/' || r == '.' || r == '_' || r == '-' || r == ':' || r == '@' || r == '=' || r == ',' || r == '+') {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
