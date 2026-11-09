package envtestfake

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// Attached is the fake ATTACHED adapter (physical-device-like): devices
// exist in the world independent of the manager, are discovered by
// serial, attached after identity verification, and only ever
// disconnected — never created or destroyed. Seed devices with AddDevice.
type Attached struct {
	World  *World
	Script *Script
	KindV  envspec.Kind
}

func NewAttached(w *World, kind envspec.Kind) *Attached {
	if kind == "" {
		kind = envspec.Android
	}
	return &Attached{World: w, Script: &Script{observes: map[string]int{}}, KindV: kind}
}

// AddDevice seeds an externally-existing device with a serial.
func (a *Attached) AddDevice(serial string) *Resource {
	r := &Resource{Name: "serial:" + serial, External: serial,
		files: map[string][]byte{}, modes: map[string]int64{},
		snaps: map[string]map[string][]byte{}, envInjected: map[string]string{}}
	a.World.mu.Lock()
	a.World.resources[r.Name] = r
	a.World.mu.Unlock()
	return r
}

func (a *Attached) script() *Script {
	if a.Script.observes == nil {
		a.Script.observes = map[string]int{}
	}
	return a.Script
}

func (a *Attached) Describe() adapter.Description {
	return adapter.Description{Name: "fake-attached", Kinds: []envspec.Kind{a.KindV}, Relationship: adapter.Attached, Version: "1"}
}

func (a *Attached) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	if s.Params["serial"] == "" {
		return fmt.Errorf("%w: attach requires params.serial", envspec.ErrInvalid)
	}
	return nil
}

func (a *Attached) Cost(s envspec.Spec) adapter.Cost {
	return adapter.Cost{Pool: envspec.PoolHost, CPUs: s.Resources.CPUs, MemoryMB: s.Resources.MemoryMB}
}

func (a *Attached) Attach(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	sc := a.script()
	sc.mu.Lock()
	gate, partial, createErr := sc.CreateGate, sc.CreatePartial, sc.CreateErr
	sc.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	serial := s.Params["serial"]
	r := a.World.Get("serial:" + serial)
	if r == nil {
		return nil, fmt.Errorf("%w: device %s not present", envspec.ErrNotFound, serial)
	}
	// Identity verification: the serial must match what the spec asked
	// for, and a device attaches to one env at a time.
	r.mu.Lock()
	if r.Attached {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: device %s already attached", envspec.ErrConflict, serial)
	}
	r.Attached = true
	r.EnvID = op.EnvID
	r.mu.Unlock()
	inst := &Instance{att: a, r: r, Device: serial}
	if partial {
		err := createErr
		if err == nil {
			err = errors.New("fake: attach died after connecting")
		}
		return inst, err
	}
	if createErr != nil {
		r.mu.Lock()
		r.Attached = false
		r.mu.Unlock()
		return nil, createErr
	}
	return inst, nil
}

func (a *Attached) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref, spec envspec.Spec) (adapter.Instance, error) {
	if sc := a.script(); sc.OpenErr != nil {
		return nil, adapter.BackendDown(sc.OpenErr)
	}
	r := a.World.Get(ref.Name)
	if r == nil {
		return nil, fmt.Errorf("%w: %s", envspec.ErrNotFound, ref.Name)
	}
	r.mu.Lock()
	r.Attached = true
	r.mu.Unlock()
	return &Instance{att: a, r: r, Device: ref.External}, nil
}

func (a *Attached) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	sc := a.script()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.DiscoverErr != nil {
		return adapter.Observation{Ref: ref, Presence: adapter.Unknown, Detail: sc.DiscoverErr.Error()}, nil
	}
	r := a.World.resources[ref.Name]
	if r == nil {
		return adapter.Observation{Ref: ref, Presence: adapter.Absent}, nil
	}
	sc.observes[ref.Name]++
	if sc.ObserveAbsentAfterN > 0 && sc.observes[ref.Name] > sc.ObserveAbsentAfterN {
		return adapter.Observation{Ref: ref, Presence: adapter.Absent}, nil
	}
	r.mu.Lock()
	attached := r.Attached
	r.mu.Unlock()
	return adapter.Observation{Ref: adapter.Ref{EnvID: r.EnvID, External: r.External, Name: r.Name},
		Presence: adapter.Present, Attached: attached, Workspace: "/data/local/tmp/work"}, nil
}

func (a *Attached) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	sc := a.script()
	sc.mu.Lock()
	derr := sc.DiscoverErr
	sc.mu.Unlock()
	if derr != nil {
		return nil, adapter.BackendDown(derr)
	}
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	var out []adapter.Candidate
	for _, r := range a.World.resources {
		out = append(out, adapter.Candidate{Ref: adapter.Ref{EnvID: r.EnvID, External: r.External, Name: r.Name}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Name < out[j].Ref.Name })
	return out, nil
}
