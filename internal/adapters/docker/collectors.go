package docker

// Collectors owned by the docker adapter, bound at construction to
// this instance's typed ports (engine client + container id). No
// moby type leaves this package. Each Run executes once and returns
// its failure; the core's activity reconciler supervises/restarts.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Collectors returns this instance's collectors (adapter.CollectorProvider).
func (i *Instance) Collectors() []signalcore.Collector {
	return []signalcore.Collector{
		&logsCollector{inst: i},
		&resourcesCollector{inst: i},
		&fsChangesCollector{inst: i},
		&fsActivityCollector{inst: i},
		&processStreamCollector{},
		&netPCAPCollector{inst: i},
		&netDerivedCollector{inst: i},
		&netHTTPCollector{inst: i},
	}
}

// CollectorEnvInfo describes this instance for applicability checks.
func (i *Instance) CollectorEnvInfo() signalcore.EnvInfo {
	return signalcore.EnvInfo{ID: i.envID, Kind: string(envspec.Container), Profile: i.spec.Profile}
}

func objSchema(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required,
		AdditionalProperties: &jsonschema.Schema{}}
}

func strSchema() *jsonschema.Schema { return &jsonschema.Schema{Type: "string"} }

// --- container.logs@1 ---

type logsCollector struct{ inst *Instance }

func (c *logsCollector) Name() string { return "container.logs" }
func (c *logsCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamContainerLogs,
		Description: "Container stdout/stderr (init command)", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.LogLine{})}}
}
func (c *logsCollector) Params() *jsonschema.Schema { return nil }
func (c *logsCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	res, err := c.inst.ad.cli.ContainerLogs(ctx, c.inst.id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Follow: true, Timestamps: false, Tail: "100",
	})
	if err != nil {
		return engineErr(err)
	}
	defer res.Close()
	err = scanDockerFrames(res, func(stream string, line string) {
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamContainerLogs,
			Data: envspec.LogLine{Text: line, Stream: stream}})
	})
	if err != nil && ctx.Err() != nil {
		return nil
	}
	if err == nil && ctx.Err() == nil {
		return fmt.Errorf("container.logs: log stream ended")
	}
	return err
}

// scanDockerFrames reads Docker's 8-byte framed log stream line-wise.
func scanDockerFrames(r io.Reader, fn func(stream, line string)) error {
	br := bufio.NewReader(r)
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:8])
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		if size == 0 {
			continue
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(br, payload); err != nil {
			return err
		}
		text := strings.TrimRight(string(payload), "\n")
		if text != "" {
			fn(stream, text)
		}
	}
}

// --- env.resources@1 (container stats) ---

type resourcesCollector struct {
	inst     *Instance
	Interval time.Duration
}

func (r *resourcesCollector) Name() string { return "env.resources" }
func (r *resourcesCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamEnvResources,
		Description: "Per-environment CPU/memory/network use", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.ResourcesData{})}}
}
func (r *resourcesCollector) Params() *jsonschema.Schema { return nil }
func (r *resourcesCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	iv := r.Interval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		d, err := r.sample(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamEnvResources, Data: d})
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *resourcesCollector) sample(ctx context.Context) (envspec.ResourcesData, error) {
	res, err := r.inst.ad.cli.ContainerStats(ctx, r.inst.id, client.ContainerStatsOptions{Stream: false})
	if err != nil {
		return envspec.ResourcesData{}, engineErr(err)
	}
	defer res.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&st); err != nil {
		return envspec.ResourcesData{}, err
	}
	d := envspec.ResourcesData{Procs: int(st.PidsStats.Current)}
	cpuDelta := float64(st.CPUStats.CPUUsage.TotalUsage) - float64(st.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(st.CPUStats.SystemUsage) - float64(st.PreCPUStats.SystemUsage)
	if sysDelta > 0 && cpuDelta >= 0 {
		cpus := float64(len(st.CPUStats.CPUUsage.PercpuUsage))
		if cpus == 0 {
			cpus = 1
		}
		d.CPUPercent = cpuDelta / sysDelta * cpus * 100
	}
	d.MemMB = float64(st.MemoryStats.Usage) / (1 << 20)
	d.MemLimitMB = float64(st.MemoryStats.Limit) / (1 << 20)
	for _, n := range st.Networks {
		d.NetRxKB += float64(n.RxBytes) / 1024
		d.NetTxKB += float64(n.TxBytes) / 1024
	}
	return d, nil
}

// --- fs.changes@1 (manifest diff; manifest via archive read) ---

type fsChangesCollector struct {
	inst     *Instance
	Interval time.Duration
}

func (f *fsChangesCollector) Name() string { return "fs.changes" }
func (f *fsChangesCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamFSChanges,
		Description: "File tree changes at checkpoints (manifest diff)", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.FSChange{})}}
}
func (f *fsChangesCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{
		"roots": {Type: "array", Items: strSchema()},
	})
}
func (f *fsChangesCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	iv := f.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	var p struct {
		Roots []string `json:"roots"`
	}
	_ = json.Unmarshal(params, &p)
	if len(p.Roots) == 0 {
		p.Roots = []string{"/work"}
	}
	prev := map[string]map[string]envspec.Entry{}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		for _, root := range p.Roots {
			man, err := f.inst.archiveManifest(ctx, root)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("fs.changes: manifest %s: %w", root, err)
			}
			cur := map[string]envspec.Entry{}
			for _, en := range man {
				cur[en.Path] = en
			}
			if old := prev[root]; old != nil {
				for _, ch := range diffEntries(old, cur, root) {
					out.Emit(envspec.SignalEvent{Stream: envspec.StreamFSChanges, Data: ch})
				}
			}
			prev[root] = cur
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func diffEntries(old, cur map[string]envspec.Entry, root string) []envspec.FSChange {
	var out []envspec.FSChange
	for p, en := range cur {
		if o, ok := old[p]; !ok {
			out = append(out, envspec.FSChange{Path: p, Change: "created", Mode: en.Mode, Size: en.Size, Digest: en.Digest, Root: root})
		} else if o.Digest != en.Digest || o.Size != en.Size || o.Mode != en.Mode {
			out = append(out, envspec.FSChange{Path: p, Change: "modified", Mode: en.Mode, Size: en.Size, Digest: en.Digest, Root: root})
		}
	}
	for p := range old {
		if _, ok := cur[p]; !ok {
			out = append(out, envspec.FSChange{Path: p, Change: "deleted", Root: root})
		}
	}
	return out
}

// archiveManifest hashes a container tree by streaming its tar
// through a digesting sink. Collector-internal: the docker adapter
// deliberately does not implement adapter.Manifestor, because
// archive paths are rooted at the copied directory's base name and
// would not match source-relative paths in a core delta push.
func (i *Instance) archiveManifest(ctx context.Context, root string) ([]envspec.Entry, error) {
	rc, err := i.GetArchive(ctx, adapter.OpContext{EnvID: i.envID}, root)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	sink := &manifestSink{}
	if _, err := xfer.ReadTar(ctx, rc, sink, xfer.Options{}); err != nil {
		return nil, err
	}
	return sink.entries, nil
}

type manifestSink struct{ entries []envspec.Entry }

func (s *manifestSink) Add(path string, mode int64, r io.Reader) error {
	d, n, err := xfer.DigestOf(r)
	if err != nil {
		return err
	}
	s.entries = append(s.entries, envspec.Entry{Path: path, Mode: mode, Size: n, Digest: d})
	return nil
}

// --- fs.activity@1 (sidecar inotifywait in the PID namespace) ---

type fsActivityCollector struct{ inst *Instance }

func (f *fsActivityCollector) Name() string { return "fs.activity" }
func (f *fsActivityCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamFSActivity,
		Description: "Live file activity (inotify/fanotify)", Cost: "med",
		Schema: signalcore.SchemaFor(envspec.FSActivityData{})}}
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
		p.Roots = []string{"/work"}
	}
	roots := make([]string, len(p.Roots))
	for idx, r := range p.Roots {
		roots[idx] = "/proc/1/root" + r
	}
	args := append([]string{"inotifywait", "-m", "-r", "-q", "--format", "%w|%e|%f",
		"-e", "create,modify,delete,moved_from,moved_to,attrib,open"}, roots...)
	rc, cleanup, err := f.inst.sidecarExec(ctx, "pid", nil, args)
	if err != nil {
		return err
	}
	defer cleanup()
	defer rc.Close()
	err = readLines(ctx, rc, func(line string) {
		if d, ok := parseInotifyLine(line); ok {
			out.Emit(envspec.SignalEvent{Stream: envspec.StreamFSActivity, Data: d})
		}
	})
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		return fmt.Errorf("fs.activity: inotifywait ended")
	}
	return err
}

func inotifyOp(events string) string {
	low := strings.ToLower(events)
	switch {
	case strings.Contains(low, "create"):
		return "create"
	case strings.Contains(low, "delete"):
		return "delete"
	case strings.Contains(low, "modify") || strings.Contains(low, "close_write"):
		return "modify"
	case strings.Contains(low, "moved"):
		return "move"
	case strings.Contains(low, "attrib"):
		return "attrib"
	case strings.Contains(low, "open"):
		return "open"
	default:
		return low
	}
}

// parseInotifyLine parses inotifywait --format '%w|%e|%f'.
func parseInotifyLine(line string) (envspec.FSActivityData, bool) {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return envspec.FSActivityData{}, false
	}
	path := strings.TrimSpace(parts[0])
	if len(parts) > 2 && parts[2] != "" {
		path = strings.TrimSuffix(path, "/") + "/" + parts[2]
	}
	path = strings.TrimPrefix(path, "/proc/1/root")
	return envspec.FSActivityData{Path: path, Op: inotifyOp(parts[1])}, true
}

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

// --- env.process@1 (virtual: the core tees Exec output) ---

type processStreamCollector struct{}

func (p *processStreamCollector) Name() string { return "env.process" }
func (p *processStreamCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamEnvProcess,
		Description: "Stdout/stderr of every Exec and daemon, keyed by proc ID", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.ProcessData{})}}
}
func (p *processStreamCollector) Params() *jsonschema.Schema { return nil }
func (p *processStreamCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	<-ctx.Done()
	return nil
}
