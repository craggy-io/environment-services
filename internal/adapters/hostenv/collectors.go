package hostenv

// Collectors owned by the host adapter, bound to this instance's
// typed ports (job accounting, in-process manifests) at
// construction. Each Run executes once and returns its failure;
// the core's activity reconciler supervises and restarts.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// Collectors returns this instance's collectors (adapter.CollectorProvider).
func (i *Instance) Collectors() []signalcore.Collector {
	return []signalcore.Collector{
		&resourcesCollector{inst: i},
		&fsChangesCollector{inst: i},
		&processStreamCollector{},
	}
}

// CollectorEnvInfo describes this instance for applicability checks.
func (i *Instance) CollectorEnvInfo() signalcore.EnvInfo {
	return signalcore.EnvInfo{ID: i.envID, Kind: string(envspec.Host), Profile: i.spec.Profile}
}

func objSchema(props map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required,
		AdditionalProperties: &jsonschema.Schema{}}
}

func strSchema() *jsonschema.Schema { return &jsonschema.Schema{Type: "string"} }

// --- env.resources@1 (host flavour: job accounting) ---

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
		cpu, procs, err := r.inst.Accounting()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("env.resources: job accounting: %w", err)
		}
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamEnvResources,
			Data: envspec.ResourcesData{Procs: int(procs), Top: fmt.Sprintf("job cpu time %s", cpu)}})
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// --- fs.changes@1 (manifest diff over the workspace) ---

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
	op := adapter.OpContext{EnvID: f.inst.envID}
	prev := map[string]map[string]envspec.Entry{}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		for _, root := range p.Roots {
			man, err := f.inst.Manifest(ctx, op, root)
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

// --- env.process@1 (virtual: the core tees Exec output) ---

// processStreamCollector exists so env.process@1 has a definition
// and pins resolve. The events are published by the core's exec
// tee; the collector body only holds the run open.
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
