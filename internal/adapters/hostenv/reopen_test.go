package hostenv_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adapters/hostenv"
)

// After the manager restarts its job object is gone. A host env none of
// whose processes still run is opened with a new job, so it can still be
// released and its workspace removed: it was stuck releasing.
func TestAHostEnvWhoseJobIsGoneCanStillBeReleased(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	ad := hostenv.Default(dir, filepath.Join(root, "work"))
	ctx := context.Background()
	id := envspec.NewID("env_")
	op := adapter.OpContext{EnvID: id, Generation: 1}
	inst, err := ad.Create(ctx, op, hostSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ws := inst.(*hostenv.Instance).LocalWorkspace()

	// As after a restart: the record names a job nobody holds.
	record := filepath.Join(dir, "host", "instances", id+".json")
	var st map[string]any
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	st["job_name"] = "craggy-gone-" + id
	b, _ = json.Marshal(st)
	if err := os.WriteFile(record, b, 0o600); err != nil {
		t.Fatal(err)
	}

	again, err := ad.Open(ctx, op, adapter.Ref{EnvID: id})
	if err != nil {
		t.Fatalf("open with its job gone: %v", err)
	}
	if err := again.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("workspace still there after release: %v", err)
	}
	_ = inst.Dispose(ctx, op, adapter.Destroy)
}
