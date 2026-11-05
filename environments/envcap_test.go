package environments_test

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	registry "github.com/craggy-io/capability-services/registry"

	"github.com/craggy-io/environment-services/envcap"
	"github.com/craggy-io/environment-services/environments"
	"github.com/craggy-io/environment-services/envspec"
)

// TestEnvCapOverRealManager is the S9 integration gate on the real
// stack: envcap.Capabilities over NewEnvCapDeps provisions a real
// host environment through the registry, and the lifecycle stamps
// carry the actual durable operation ids from the core store —
// provision and release operations differ, and neither is the
// registry job id.
func TestEnvCapOverRealManager(t *testing.T) {
	mgr := openTestManager(t)
	ctx := context.Background()
	owner := envspec.Owner{ID: "agent:test", Session: "s9"}

	reg := registry.New()
	t.Cleanup(reg.Close)
	if err := reg.Add(envcap.Capabilities(envcap.Deps{
		Manager: environments.NewEnvCapDeps(mgr),
		Owner:   func(context.Context) (envspec.Owner, error) { return owner, nil },
	})...); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddGroup(envcap.Groups()...); err != nil {
		t.Fatal(err)
	}
	invoke := func(name string, in map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(in)
		out, err := reg.Invoke(ctx, name, b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		return m
	}

	prov := invoke("env.provision", map[string]any{"profile": "host-tools", "key": "s9"})
	envID, _ := prov["env"].(string)
	provOp, _ := prov["operation_id"].(string)
	if !strings.HasPrefix(provOp, "op_") {
		t.Fatalf("provision operation_id = %q, want a durable op id", provOp)
	}
	if prov["job_id"] == nil || prov["job_id"] == "" || prov["job_id"] == provOp {
		t.Fatalf("provision job_id = %v, want a distinct registry job", prov["job_id"])
	}
	if prov["generation"] == nil || prov["generation"].(float64) < 1 {
		t.Fatalf("provision generation = %v", prov["generation"])
	}

	argv := []string{"sh", "-c", "echo via-capability"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "echo via-capability"}
	}
	ex := invoke("env.exec", map[string]any{"env": envID, "argv": argv})
	if !strings.Contains(ex["stdout"].(string), "via-capability") || ex["exit"].(float64) != 0 {
		t.Fatalf("env.exec = %v", ex)
	}
	if ex["proc"] == "" {
		t.Fatalf("env.exec lost the proc id: %v", ex)
	}

	// Vault and signals surfaces answer over the same deps.
	if got := invoke("vault.creds.list", map[string]any{}); got["creds"] == nil {
		t.Fatalf("vault.creds.list = %v", got)
	}
	if got := invoke("signals.streams.list", map[string]any{"env": envID}); got["streams"] == nil {
		t.Fatalf("signals.streams.list = %v", got)
	}
	pin := invoke("signals.pin", map[string]any{"env": envID, "stream": "env.process@1"})
	if pin["pin"] == "" || pin["running"] != true {
		t.Fatalf("signals.pin = %v", pin)
	}
	invoke("signals.unpin", map[string]any{"pin": pin["pin"]})

	rel := invoke("env.release", map[string]any{"env": envID})
	relOp, _ := rel["operation_id"].(string)
	if !strings.HasPrefix(relOp, "op_") || relOp == provOp {
		t.Fatalf("release operation_id = %q, want its own durable op (provision was %q)", relOp, provOp)
	}
}
