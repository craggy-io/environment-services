package environmentservices_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/craggy-io/foundation-kit/archtest"
)

// allow returns a rule that lets the from packages import only the listed
// in-module packages (execution plan §4). Anything else in the module is
// forbidden. Test files aren't checked: test-only helpers such as
// internal/envtestfake are reached from _test.go files.
func allow(from []string, imports ...string) archtest.Rule {
	return archtest.Rule{
		Name:   "§4 import matrix for " + strings.Join(from, ", "),
		Why:    "add the import to the §4 matrix in the execution plan first, then here",
		From:   from,
		Forbid: []string{"./..."},
		Allow:  imports,
	}
}

// TestLayering enforces the import matrix of execution plan §4: the
// in-module allowlist plus the external confinements.
func TestLayering(t *testing.T) {
	archtest.Check(t, archtest.Config{
		// Every package must be listed, so a new one has to be placed in the
		// matrix, and a missing one fails rather than passing vacuously.
		Packages: []string{
			"./envspec", "./environments", "./envcap", "./envtest",
			"./examples/demo", "./cmd/xfer-agent",
			"./internal/adapter", "./internal/adb", "./internal/androiddev",
			"./internal/adapters/androidemu", "./internal/adapters/androidphys",
			"./internal/adapters/docker", "./internal/adapters/hostenv",
			"./internal/core", "./internal/envtestfake", "./internal/hostmon",
			"./internal/rules", "./internal/signalcore", "./internal/store",
			"./internal/vault", "./internal/xfer",
		},
		Rules: []archtest.Rule{
			allow([]string{"./envspec", "./internal/adb"}),
			allow([]string{"./envcap"}, "./envspec"),
			allow([]string{"./environments"}, "./envspec", "./envcap",
				"./internal/adapter", "./internal/adapters/...",
				"./internal/androiddev", "./internal/core", "./internal/hostmon", "./internal/rules",
				"./internal/signalcore", "./internal/store", "./internal/vault", "./internal/xfer"),
			allow([]string{"./envtest"}, "./envspec", "./environments", "./internal/signalcore"),
			allow([]string{"./examples/demo"}, "./envspec", "./environments", "./envcap"),
			allow([]string{"./cmd/xfer-agent"}, "./internal/xfer"),
			allow([]string{"./internal/adapter", "./internal/rules"}, "./envspec", "./internal/signalcore"),
			allow([]string{"./internal/hostmon", "./internal/store", "./internal/vault",
				"./internal/xfer", "./internal/signalcore"}, "./envspec"),
			allow([]string{"./internal/androiddev"},
				"./envspec", "./internal/adapter", "./internal/adb", "./internal/signalcore"),
			allow([]string{"./internal/adapters/docker", "./internal/adapters/hostenv", "./internal/envtestfake"},
				"./envspec", "./internal/adapter", "./internal/signalcore", "./internal/xfer"),
			allow([]string{"./internal/adapters/androidemu", "./internal/adapters/androidphys"},
				"./envspec", "./internal/adapter", "./internal/adb", "./internal/androiddev", "./internal/signalcore"),
			allow([]string{"./internal/core"}, "./envspec", "./internal/adapter", "./internal/hostmon",
				"./internal/signalcore", "./internal/store", "./internal/vault", "./internal/xfer"),
			{
				// Consumers' domain packages alias envspec's record types, so it
				// must not pull drivers, SQLite or Docker clients transitively.
				// It imports nothing in-module, so stdlib-only here is transitive.
				Name:   "envspec is stdlib-only",
				Why:    "envspec is load-bearing for consumers (execution plan §3.1, §4)",
				From:   []string{"./envspec"},
				Forbid: []string{archtest.External},
			},
			{
				Name:   "envtest is test-only",
				Why:    "no production package imports envtest",
				From:   []string{"./..."},
				Forbid: []string{"./envtest/..."},
			},
			{
				Name:   "no artifact-services",
				Why:    "environment-services has zero artifact-services dependency",
				From:   []string{"./..."},
				Forbid: []string{"github.com/craggy-io/artifact-services/..."},
			},
			{
				Name:   "Starlark only in internal/rules",
				Why:    "script-services and go.starlark.net are confined to internal/rules",
				From:   []string{"./..."},
				Except: []string{"./internal/rules"},
				Forbid: []string{"github.com/craggy-io/script-services/...", "go.starlark.net/..."},
			},
			{
				Name:   "capability-services only in envcap",
				Why:    "capability-services is confined to envcap and the demo",
				From:   []string{"./..."},
				Except: []string{"./envcap", "./examples/demo"},
				Forbid: []string{"github.com/craggy-io/capability-services/..."},
			},
			{
				Name:   "container and packet technology only in the docker adapter",
				Why:    "moby and gopacket are confined to internal/adapters/docker",
				From:   []string{"./..."},
				Except: []string{"./internal/adapters/docker"},
				Forbid: []string{"github.com/moby/moby/...", "github.com/gopacket/gopacket/..."},
			},
		},
	})
}

// TestNoArtifactServicesModule checks the module graph itself, which
// archtest can't see.
func TestNoArtifactServicesModule(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "all").Output()
	if err != nil {
		t.Skipf("go list -m unavailable: %v", err)
	}
	if strings.Contains(string(out), "artifact-services") {
		t.Errorf("module graph contains artifact-services:\n%s", out)
	}
}
