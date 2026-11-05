package environmentservices_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

const modulePath = "github.com/craggy-io/environment-services"

// envspec must import only the standard library: consumers' domain packages
// alias its record types and must not pull drivers, SQLite or Docker
// clients transitively. This is load-bearing (execution plan §3.1, §4).
func TestEnvSpecStdlibOnly(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}",
		modulePath+"/envspec").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == modulePath+"/envspec" {
			continue
		}
		// Any dotted first path element is external (stdlib has none).
		if first, _, _ := strings.Cut(line, "/"); strings.Contains(first, ".") {
			t.Errorf("envspec depends on external package %s", line)
		}
	}
}

type pkgInfo struct {
	ImportPath string
	Imports    []string
}

func listPackages(t *testing.T) []pkgInfo {
	t.Helper()
	out, err := exec.Command("go", "list", "-json", "./...").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	var pkgs []pkgInfo
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func importsAny(imports []string, prefixes ...string) (string, bool) {
	for _, imp := range imports {
		for _, pre := range prefixes {
			if imp == pre || strings.HasPrefix(imp, pre+"/") {
				return imp, true
			}
		}
	}
	return "", false
}

// allowedModuleImports is the full in-module import matrix (execution
// plan §4): for each package (module-relative path), the in-module
// packages it may import directly. Anything not listed is forbidden.
// Test files are not covered (go list .Imports excludes them): test-only
// helpers such as internal/envtestfake are reached from _test.go files.
func allowedModuleImports(rel string) []string {
	switch rel {
	case "", "envspec", "internal/adb":
		return nil
	case "envcap":
		return []string{"envspec"}
	case "environments":
		return []string{"envspec", "envcap", "internal/adapter", "internal/adapters",
			"internal/androiddev", "internal/core", "internal/hostmon", "internal/rules",
			"internal/signalcore", "internal/store", "internal/vault", "internal/xfer"}
	case "envtest":
		return []string{"envspec", "environments", "internal/signalcore"}
	case "examples/demo":
		return []string{"envspec", "environments", "envcap"}
	case "cmd/xfer-agent":
		return []string{"internal/xfer"}
	case "internal/adapter":
		return []string{"envspec", "internal/signalcore"}
	case "internal/hostmon", "internal/store", "internal/vault",
		"internal/xfer", "internal/signalcore":
		return []string{"envspec"}
	case "internal/androiddev":
		return []string{"envspec", "internal/adapter", "internal/adb", "internal/signalcore"}
	case "internal/adapters/docker", "internal/adapters/hostenv", "internal/envtestfake":
		return []string{"envspec", "internal/adapter", "internal/signalcore", "internal/xfer"}
	case "internal/adapters/androidemu", "internal/adapters/androidphys":
		return []string{"envspec", "internal/adapter", "internal/adb", "internal/androiddev", "internal/signalcore"}
	case "internal/core":
		return []string{"envspec", "internal/adapter", "internal/hostmon",
			"internal/signalcore", "internal/store", "internal/vault", "internal/xfer"}
	case "internal/rules":
		return []string{"envspec", "internal/signalcore"}
	}
	return nil
}

// requiredPackages must all exist, so the matrix can never pass
// vacuously over a partial tree.
var requiredPackages = []string{
	"envspec", "environments", "envcap", "envtest",
	"examples/demo", "cmd/xfer-agent",
	"internal/adapter", "internal/adb", "internal/androiddev",
	"internal/adapters/androidemu", "internal/adapters/androidphys",
	"internal/adapters/docker", "internal/adapters/hostenv",
	"internal/core", "internal/envtestfake", "internal/hostmon",
	"internal/rules", "internal/signalcore", "internal/store",
	"internal/vault", "internal/xfer",
}

// TestLayering enforces the full import matrix of execution plan §4:
// the in-module allowlist above plus the external confinements
// (artifact-services nowhere; script-services and go.starlark.net only
// in internal/rules; capability-services only in envcap and the demo;
// moby/gopacket only in the docker adapter; envtest imported by no
// production package).
func TestLayering(t *testing.T) {
	rel := func(path string) string {
		return strings.TrimPrefix(strings.TrimPrefix(path, modulePath), "/")
	}
	pkgs := listPackages(t)
	present := map[string]bool{}
	for _, p := range pkgs {
		present[rel(p.ImportPath)] = true
	}
	for _, r := range requiredPackages {
		if !present[r] {
			t.Errorf("required package %s missing; layering matrix would be vacuous", r)
		}
	}
	for _, p := range pkgs {
		r := rel(p.ImportPath)
		fail := func(rule, imp string) {
			t.Errorf("%s imports %s: forbidden (%s)", p.ImportPath, imp, rule)
		}

		// In-module allowlist.
		allowed := allowedModuleImports(r)
		for _, imp := range p.Imports {
			if !strings.HasPrefix(imp, modulePath+"/") {
				continue
			}
			ir := rel(imp)
			ok := false
			for _, a := range allowed {
				if ir == a || strings.HasPrefix(ir, a+"/") {
					ok = true
					break
				}
			}
			if !ok {
				fail("not in the §4 import matrix for "+r, imp)
			}
			if ir == "envtest" || strings.HasPrefix(ir, "envtest/") {
				fail("envtest is test-only and imported by no production package", imp)
			}
		}

		// No artifact-services dependency anywhere.
		if imp, bad := importsAny(p.Imports, "github.com/craggy-io/artifact-services"); bad {
			fail("environment-services has zero artifact-services dependency", imp)
		}
		// script-services and Starlark only inside internal/rules.
		if r != "internal/rules" {
			if imp, bad := importsAny(p.Imports,
				"github.com/craggy-io/script-services", "go.starlark.net"); bad {
				fail("script-services/go.starlark.net only in internal/rules", imp)
			}
		}
		// capability-services only in envcap and the demo.
		if r != "envcap" && r != "examples/demo" {
			if imp, bad := importsAny(p.Imports, "github.com/craggy-io/capability-services"); bad {
				fail("capability-services only in envcap (+demo)", imp)
			}
		}
		// Container/packet technology only in the docker adapter.
		if r != "internal/adapters/docker" {
			if imp, bad := importsAny(p.Imports,
				"github.com/moby/moby", "github.com/gopacket/gopacket"); bad {
				fail("moby/gopacket only in internal/adapters/docker", imp)
			}
		}
	}

	// The module graph itself must not contain artifact-services.
	out, err := exec.Command("go", "list", "-m", "all").Output()
	if err != nil {
		t.Skipf("go list -m unavailable: %v", err)
	}
	if strings.Contains(string(out), "artifact-services") {
		t.Errorf("module graph contains artifact-services:\n%s", out)
	}
}
