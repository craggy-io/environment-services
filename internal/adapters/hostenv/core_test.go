package hostenv_test

// Core-driven smoke: the durable core provisions, execs, pushes,
// pulls and releases a real host environment (real workspace, real
// job, real processes) through the adapter contract.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adapters/hostenv"
	"github.com/craggy-io/environment-services/internal/core"
)

func TestCoreProvisionsHostEnvironment(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	ad := hostenv.Default(dir, filepath.Join(root, "work"))
	m, err := core.Open(context.Background(), core.Config{
		Dir: dir, Adapters: []adapter.Adapter{ad},
		HealthInterval: 100 * time.Millisecond, ReapInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close(context.Background(), core.ShutdownDrain) }()
	ctx := context.Background()

	h, err := m.Provision(ctx, envspec.Owner{ID: "agent:test"}, envspec.Spec{Profile: "host-tools"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: echoArgv("core-host-ok")})
	if err != nil || code != 0 || !strings.Contains(string(out), "core-host-ok") {
		t.Fatalf("exec: %q code=%d err=%v", out, code, err)
	}
	if err := h.Push(ctx, envspec.MemSource(map[string][]byte{"n.txt": []byte("7")}), "/work"); err != nil {
		t.Fatal(err)
	}
	sink := envspec.NewMemSink()
	if err := h.Pull(ctx, "/work", sink); err != nil {
		t.Fatal(err)
	}
	if string(sink.Files["n.txt"]) != "7" {
		t.Fatalf("pull = %v", sink.Files)
	}
	envID := h.ID()
	if err := m.Release(ctx, envID); err != nil {
		t.Fatal(err)
	}
	info, err := m.Info(envID)
	if err != nil || info.State != envspec.StateReleased {
		t.Fatalf("after release: %+v %v", info, err)
	}
	ws := filepath.Join(root, "work", envID, "work")
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace %s must be deleted after release (stat err %v)", ws, err)
	}
}
