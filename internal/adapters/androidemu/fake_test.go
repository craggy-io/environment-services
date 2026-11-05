package androidemu

import (
	"context"
	"io"
	"strings"

	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

// transportOver adapts the test's bootFake to androiddev.Transport.
type transportOver struct{ f *bootFake }

func (t *transportOver) Run(ctx context.Context, serial, cmd string) (string, int, error) {
	t.f.runs = append(t.f.runs, cmd)
	switch {
	case cmd == "echo ok":
		return "ok", 0, nil
	case strings.Contains(cmd, "id"):
		return "uid=0(root)", 0, nil
	case strings.Contains(cmd, "getprop sys.boot_completed"):
		return "1", 0, nil
	default:
		return "", 0, nil
	}
}

func (t *transportOver) Shell(ctx context.Context, serial, cmd string) (*androiddev.ShellSession, error) {
	out, code, err := t.Run(ctx, serial, cmd)
	if err != nil {
		return nil, err
	}
	return &androiddev.ShellSession{Stdout: strings.NewReader(out), Stderr: strings.NewReader(""),
		Stdin: nopWC{}, Wait: func() (int, error) { return code, nil }, Kill: func() error { return nil }}, nil
}

type nopWC struct{}

func (nopWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopWC) Close() error                { return nil }

func (t *transportOver) ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error) {
	out, _, err := t.Run(ctx, serial, cmd)
	return io.NopCloser(strings.NewReader(out)), func() error { return nil }, err
}
func (t *transportOver) SyncSend(ctx context.Context, serial, p string, m uint32, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
func (t *transportOver) Forward(ctx context.Context, serial string, l, r int) (int, error) {
	if l == 0 {
		return 47000, nil
	}
	return l, nil
}
func (t *transportOver) Devices(ctx context.Context) ([]adb.Device, error) {
	return t.f.Devices(ctx)
}
func (t *transportOver) Root(ctx context.Context, serial string) (string, error) {
	return "restarting adbd as root", nil
}
func (t *transportOver) KillServer(ctx context.Context) error { return nil }
func (t *transportOver) Version(ctx context.Context) (string, error) {
	return "0029", t.f.devErr
}
func (t *transportOver) Emu(ctx context.Context, serial, cmd string) (string, error) {
	if t.f.emuOut != nil {
		if v, ok := t.f.emuOut[cmd]; ok {
			return v, nil
		}
	}
	return "OK", nil
}

var _ androiddev.Transport = (*transportOver)(nil)
