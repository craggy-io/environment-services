// Command demo is the assembly smoke test for environment-services:
// it opens a Manager through environments.Open (the one assembly
// root), provisions a host environment, pushes a file, execs, pulls
// the result back, pages lifecycle events, registers the envcap
// capability surface into a capability-services registry and invokes
// a few capabilities against the same manager, then drains. It
// imports only the public packages a consumer may use.
//
//	go run ./examples/demo
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	registry "github.com/craggy-io/capability-services/registry"

	"github.com/craggy-io/environment-services/envcap"
	"github.com/craggy-io/environment-services/environments"
	"github.com/craggy-io/environment-services/envspec"
)

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func main() {
	var root string
	flag.StringVar(&root, "dir", "", "state directory (default: temp dir, removed on exit)")
	flag.Parse()
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "craggy-demo-*")
		fatal(err)
		defer os.RemoveAll(root)
	}
	dir := filepath.Join(root, "state")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fmt.Println("state dir:", dir)

	mgr, err := environments.Open(ctx, environments.Config{Dir: dir, ManagerID: "mgr_demo"})
	fatal(err)
	fmt.Println("manager:", mgr.ManagerID())
	for _, d := range mgr.Drivers() {
		fmt.Printf("  driver %s kinds=%v version=%s\n", d.Name, d.Kinds, d.Version)
	}
	snap := mgr.HostSnapshot()
	fmt.Printf("host: %d cpus, %d MB free\n", snap.CPUs, snap.MemFreeMB)
	if st := mgr.ProfileStatus("host-tools"); !st.Ready {
		fatal(fmt.Errorf("host-tools not ready: %s", st.NotReady))
	}

	owner := envspec.Owner{ID: "user:demo", Session: "demo-session"}
	h, err := mgr.Provision(ctx, owner, envspec.Spec{Profile: "host-tools", Key: "demo-host"},
		environments.OnProgress(func(p envspec.Progress) { fmt.Println("  progress:", p.Stage, p.Message) }))
	fatal(err)
	fmt.Println("host env:", h.ID(), h.Info().State)

	fatal(h.Push(ctx, envspec.MemSource(map[string][]byte{"hello.txt": []byte("hello craggy\n")}), "/work/in"))
	argv := []string{"sh", "-c", "cat in/hello.txt && echo world > out.txt"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "type in\\hello.txt && echo world> out.txt"}
	}
	out, serr, code, err := h.Exec(ctx, envspec.Cmd{Argv: argv})
	fatal(err)
	fmt.Printf("  exec exit=%d stdout=%q stderr=%q\n", code, out, serr)

	sink := envspec.NewMemSink()
	fatal(h.Pull(ctx, "/work", sink))
	names := make([]string, 0, len(sink.Files))
	for n := range sink.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("  pulled %d files: %v\n", len(names), names)
	for _, n := range names {
		if filepath.Base(n) == "out.txt" {
			fmt.Printf("  pulled out.txt=%q\n", string(sink.Files[n]))
		}
	}

	// The capability surface: same manager, projected through a
	// capability-services registry. Artifacts stay nil (no artifact
	// bridge in the demo), so artifact-producing paths degrade.
	reg := registry.New()
	defer reg.Close()
	fatal(reg.Add(envcap.Capabilities(envcap.Deps{
		Manager: environments.NewEnvCapDeps(mgr),
		Owner:   func(context.Context) (envspec.Owner, error) { return owner, nil },
	})...))
	fatal(reg.AddGroup(envcap.Groups()...))
	fmt.Printf("capabilities registered: %d in %d groups\n", len(reg.List()), len(reg.Groups()))
	invoke := func(name string, in map[string]any) map[string]any {
		b, _ := json.Marshal(in)
		out, err := reg.Invoke(ctx, name, b)
		fatal(err)
		var m map[string]any
		fatal(json.Unmarshal(out, &m))
		return m
	}
	status := invoke("env.status", map[string]any{})
	fmt.Printf("  env.status envs=%d collectors=%v\n", len(status["envs"].([]any)), status["collectors"])
	hs := invoke("host.status", map[string]any{})
	fmt.Printf("  host.status pools=%d\n", len(hs["pools"].([]any)))
	ex := invoke("env.exec", map[string]any{"env": h.ID(), "argv": argv})
	fmt.Printf("  env.exec exit=%v proc=%v stdout=%q\n", ex["exit"], ex["proc"] != "", ex["stdout"])

	evs, err := mgr.EventsSince(ctx, 0, 50)
	fatal(err)
	fmt.Printf("lifecycle events recorded: %d (first kinds: ", len(evs))
	for i, ev := range evs {
		if i >= 5 {
			break
		}
		fmt.Printf("%s ", ev.Kind)
	}
	fmt.Println(")")

	fatal(mgr.Release(ctx, h.ID()))
	fatal(mgr.Close(ctx, environments.ShutdownDrain))
	fmt.Println("demo OK")
}
