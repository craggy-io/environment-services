package androiddev

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// The Android collectors live with the device they observe (each
// adapter owns its collectors; there is no generic collect package).
// Every collector is bound to a *Device at construction — typed
// ports only, no stringly handle lookup — runs once, and returns its
// failure; core's activity reconciler restarts it with backoff.
// Payload types are the frozen envspec records; schemas are derived
// by reflection here.

// Collectors returns every Android collector bound to this device.
func (d *Device) Collectors() []signalcore.Collector {
	return []signalcore.Collector{
		&logcatCollector{dev: d},
		&procCollector{dev: d},
		&packagesCollector{dev: d},
		&crashCollector{dev: d},
		&stateCollector{dev: d},
		&memoryCollector{dev: d},
		&fsStateCollector{dev: d},
		&resourcesCollector{dev: d},
		&healthCollector{dev: d},
		&binderCollector{dev: d},
		&ipcCollector{dev: d},
		&fridaCollector{dev: d},
		&fsActivityCollector{dev: d},
		&pcapCollector{dev: d},
		&processCollector{},
	}
}

// CollectorEnvInfo describes this device for collector applicability.
func (d *Device) CollectorEnvInfo(envID, profile string) signalcore.EnvInfo {
	return signalcore.EnvInfo{ID: envID, Kind: "android", Profile: profile, Root: d.Rooted()}
}

// guestCmd is the collectors' command shape (argv + optional root).
type guestCmd struct {
	Argv []string
	User string // "root" wraps; anything else is the shell user
}

// execGuest runs one guest command (the collectors' exec path).
func (d *Device) execGuest(ctx context.Context, c guestCmd) (*guestProc, error) {
	return d.ExecArgv(ctx, c.Argv, c.User, "", nil, nil, 0)
}

func objSchema(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required,
		AdditionalProperties: &jsonschema.Schema{}}
}

func strSchema() *jsonschema.Schema { return &jsonschema.Schema{Type: "string"} }

func kindOnly(stream, kind string) error {
	return fmt.Errorf("%s is %s-only", stream, kind)
}

func rootHint(stream, profile string) error {
	return fmt.Errorf("%s needs root; profile %q is unrooted — provision \"android\" or \"android-play-rooted\"", stream, profile)
}

// readLines feeds collector line parsers.
func readLines(ctx context.Context, r io.Reader, fn func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		fn(sc.Text())
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return sc.Err()
}

// guestExecLines runs one long-lived shell command and streams its
// stdout lines once. When the command ends while ctx lives, that is
// the collector's failure (core restarts it) — collectors carry no
// retry loops of their own.
func guestExecLines(ctx context.Context, dev *Device, cmdline string, fn func(string)) error {
	return execLinesOnce(ctx, dev, guestCmd{Argv: []string{"sh", "-c", cmdline}}, fn)
}

func guestExecLinesRoot(ctx context.Context, dev *Device, cmdline string, fn func(string)) error {
	return execLinesOnce(ctx, dev, guestCmd{Argv: []string{"sh", "-c", cmdline}, User: "root"}, fn)
}

func execLinesOnce(ctx context.Context, dev *Device, c guestCmd, fn func(string)) error {
	proc, err := dev.execGuest(ctx, c)
	if err != nil {
		return err
	}
	defer proc.Kill()
	if proc.Stderr != nil {
		go io.Copy(io.Discard, proc.Stderr)
	}
	rerr := readLines(ctx, proc.Stdout, fn)
	code, werr := proc.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if werr != nil {
		return werr
	}
	if rerr != nil {
		return rerr
	}
	return fmt.Errorf("guest command %q ended (exit %d)", strings.Join(c.Argv, " "), code)
}

// rootCheck is the shared prefix of root preflight scripts: it prints
// why the shell is not root and stops, or falls through to the next
// check. Scripts built on it must print "ok" on success.
const rootCheck = `if [ "$(id -u)" != 0 ]; then echo "the root shell is unavailable (id -u is $(id -u)); the device is not rooted or su was refused"; exit 0; fi
`

// probe runs a short check script on the device and turns its stdout
// into the preflight answer: "ok" passes, anything else is the reason.
func probeScript(ctx context.Context, dev *Device, user, script string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	proc, err := dev.execGuest(ctx, guestCmd{Argv: []string{"sh", "-c", script}, User: user})
	if err != nil {
		return fmt.Errorf("the check could not run on the environment: %w", err)
	}
	if proc.Stderr != nil {
		go io.Copy(io.Discard, proc.Stderr)
	}
	out, _ := io.ReadAll(io.LimitReader(proc.Stdout, 4096))
	_, _ = proc.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("the check timed out on the environment")
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "ok":
		return nil
	case "":
		return fmt.Errorf("the check printed nothing (the %s shell may be unavailable)", user)
	default:
		return errors.New(s)
	}
}

// --- env.resources@1 (android: guest top excerpt) ---

type resourcesCollector struct {
	dev      *Device
	Interval time.Duration
}

func (r *resourcesCollector) Name() string { return "env.resources" }
func (r *resourcesCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamEnvResources, Description: "Per-environment CPU/memory/network use", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.ResourcesData{})}}
}
func (r *resourcesCollector) Applies(e signalcore.EnvInfo) error { return nil }
func (r *resourcesCollector) Params() *jsonschema.Schema         { return nil }
func (r *resourcesCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if d, err := sampleAndroidResources(ctx, r.dev); err == nil {
			out.Emit(envspec.SignalEvent{Stream: envspec.StreamEnvResources, Data: d})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// --- env.process@1 (definition holder; core tees Exec output) ---

type processCollector struct{}

func (p *processCollector) Name() string { return "env.process" }
func (p *processCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamEnvProcess, Description: "Stdout/stderr of every Exec and daemon, keyed by proc ID", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.ProcessData{})}}
}
func (p *processCollector) Applies(e signalcore.EnvInfo) error { return nil }
func (p *processCollector) Params() *jsonschema.Schema         { return nil }
func (p *processCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	<-ctx.Done() // published by core's exec tee, see doc above
	return nil
}

// --- env.health@1: component transitions, polled from the supervisor ---

type healthCollector struct {
	dev      *Device
	Interval time.Duration
}

func (h *healthCollector) Name() string { return "env.health" }
func (h *healthCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamEnvHealth, Description: "Component state changes, failures and repairs", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.HealthEvent{})}}
}
func (h *healthCollector) Applies(e signalcore.EnvInfo) error { return nil }
func (h *healthCollector) Params() *jsonschema.Schema         { return nil }
func (h *healthCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	iv := h.Interval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	prev := map[string]string{}
	emit := func() {
		for _, c := range h.dev.Components() {
			key := c.State + "\x00" + c.LastFailure
			if prev[c.Name] == key {
				continue
			}
			prev[c.Name] = key
			out.Emit(envspec.SignalEvent{Stream: envspec.StreamEnvHealth,
				Data: envspec.HealthEvent{Component: c.Name, State: c.State, Detail: c.LastFailure}})
		}
	}
	emit()
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			emit()
		}
	}
}

// --- fs.activity@1 (android: inotifyd as root) ---

type fsActivityCollector struct{ dev *Device }

func (f *fsActivityCollector) Name() string { return "fs.activity" }
func (f *fsActivityCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamFSActivity, Description: "Live file activity (inotifyd)", Cost: "med",
		Schema: signalcore.SchemaFor(envspec.FSActivityData{})}}
}
func (f *fsActivityCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamFSActivity, "android")
	}
	if !e.Root {
		return rootHint(envspec.StreamFSActivity, e.Profile)
	}
	return nil
}
func (f *fsActivityCollector) Preflight(ctx context.Context) error {
	return probeScript(ctx, f.dev, "root", rootCheck+`if ! command -v inotifyd >/dev/null 2>&1; then echo "inotifyd is not on this device's PATH (toybox without inotifyd)"
else echo ok; fi`)
}
func (f *fsActivityCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{"roots": {Type: "array", Items: strSchema()}})
}
func (f *fsActivityCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	var p struct {
		Roots []string `json:"roots"`
	}
	_ = json.Unmarshal(params, &p)
	if len(p.Roots) == 0 {
		p.Roots = []string{f.dev.Workspace()}
	}
	// inotifyd prints "<mask> <path> [<file>]"; one instance per root.
	// The first watcher to die ends the run (core restarts it).
	errCh := make(chan error, len(p.Roots))
	for _, root := range p.Roots {
		root := root
		go func() {
			errCh <- guestExecLines(ctx, f.dev, "inotifyd - "+root+" 2>/dev/null", func(line string) {
				if d, ok := parseInotifydLine(line, root); ok {
					out.Emit(envspec.SignalEvent{Stream: envspec.StreamFSActivity, Data: d})
				}
			})
		}()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// parseInotifydLine parses busybox/toybox inotifyd output "events path file".
func parseInotifydLine(line, root string) (envspec.FSActivityData, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return envspec.FSActivityData{}, false
	}
	maskMap := map[byte]string{'c': "create", 'm': "modify", 'd': "delete", 'o': "open", 'x': "attrib", '0': "move", 'e': "move"}
	op := "activity"
	if len(f[0]) > 0 {
		if o, ok := maskMap[f[0][0]]; ok {
			op = o
		}
	}
	path := f[1]
	if len(f) > 2 {
		path = strings.TrimSuffix(path, "/") + "/" + f[2]
	}
	return envspec.FSActivityData{Path: path, Op: op, Root: root}, true
}

// --- net.pcap (android: guest tcpdump as root, raw bytes) ---

type pcapCollector struct{ dev *Device }

func (p *pcapCollector) Name() string { return "net.pcap" }
func (p *pcapCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamNetPCAP, Description: "Raw packet capture (guest tcpdump)", Cost: "high", Bytes: true, Format: "pcap"}}
}
func (p *pcapCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamNetPCAP, "android")
	}
	if !e.Root {
		return rootHint(envspec.StreamNetPCAP, e.Profile)
	}
	return nil
}
func (p *pcapCollector) Preflight(ctx context.Context) error {
	return probeScript(ctx, p.dev, "root", rootCheck+`if ! command -v tcpdump >/dev/null 2>&1; then echo "tcpdump is not on this device's PATH"
else echo ok; fi`)
}
func (p *pcapCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{"interface": strSchema(), "filter": strSchema()})
}
func (p *pcapCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	var pp struct {
		Interface string `json:"interface"`
		Filter    string `json:"filter"`
	}
	_ = json.Unmarshal(params, &pp)
	iface := pp.Interface
	if iface == "" {
		iface = "any"
	}
	cmdline := p.dev.RootCmd("tcpdump -i " + iface + " -U -w - " + pp.Filter)
	rc, closeFn, err := p.dev.tr.ExecOut(ctx, p.dev.cfg.Serial, cmdline, nil)
	if err != nil {
		return fmt.Errorf("net.pcap: %w", err)
	}
	defer closeFn()
	buf := make([]byte, 32<<10)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			out.Emit(envspec.SignalEvent{Stream: envspec.StreamNetPCAP, Data: chunk})
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("net.pcap: tcpdump stream ended: %w", err)
		}
	}
}
