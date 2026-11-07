package envcap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	registry "github.com/craggy-io/capability-services/registry"

	"github.com/craggy-io/environment-services/envspec"
)

// Deps wires the capability set. Only three fields: the manager
// surface, the artifact bridge (nil = artifact-producing paths
// degrade to inline/count-only results), and the owner resolver.
type Deps struct {
	Manager   Manager
	Artifacts Artifacts
	// Owner maps trusted invocation context to env ownership.
	// Capability arguments never set it.
	Owner func(ctx context.Context) (envspec.Owner, error)
}

// capability is one definition in the package's internal shape;
// Capabilities converts each to a registry.Capability.
type capability struct {
	Name        string
	Version     int
	Description string // says when to choose it (agents read this)
	Effects     string // "readonly", "mutating", "long"
	Tags        []string
	Input       *jsonschema.Schema
	Output      *jsonschema.Schema
	Check       func(ctx context.Context) error
	Handler     func(ctx context.Context, call *call) (any, error)
}

// call is one invocation: raw JSON input plus progress plumbing.
type call struct {
	Input    json.RawMessage
	Progress func(envspec.Progress)
	opid     string // durable op id, stamped by lifecycle handlers
}

// set carries the shared dependencies every handler closes over.
type set struct{ deps Deps }

// Capabilities returns the full capability set as registry
// capabilities, ready for Registry.Add.
func Capabilities(d Deps) []registry.Capability {
	s := &set{deps: d}
	built := s.build()
	out := make([]registry.Capability, 0, len(built))
	for _, c := range built {
		out = append(out, s.toRegistry(c))
	}
	return out
}

// Groups returns the registry groups for Capabilities, ready for
// Registry.AddGroup: one per name prefix registered under. None
// loads always; a host decides what its agents start with.
func Groups() []registry.Group {
	return []registry.Group{
		{Name: "env", Description: "Provision, inspect and release isolated environments (host, Docker, Android emulator); run commands, move files and snapshot state in them."},
		{Name: "env.android", Description: "Drive an Android emulator you hold: install APKs, tap and type, take screenshots and attach Frida hooks."},
		{Name: "host", Description: "Status and capacity of the machine environments run on."},
		{Name: "rules", Description: "Load, test and wait on Starlark rules that trigger on recorded signals."},
		{Name: "signals", Description: "Record, query, tail and capture runtime signals (logcat, network flows, binder, crashes) from environments."},
		{Name: "vault", Description: "List and lease stored credentials for an environment without revealing secrets."},
	}
}

// toRegistry converts one internal capability: string effects become
// registry effect bits (+ Long), envspec.Progress maps onto the
// running job's progress updates (stage-scoped counters verbatim in
// Data, never flattened), and handler errors become coded registry
// errors with the hint folded into the message.
func (s *set) toRegistry(c capability) registry.Capability {
	rc := registry.Capability{
		Name:        c.Name,
		Version:     c.Version,
		Description: c.Description,
		Tags:        c.Tags,
		Check:       c.Check,
		Input:       c.Input,
		Output:      c.Output,
	}
	switch c.Effects {
	case "readonly":
		rc.Effects = registry.ReadOnly | registry.Idempotent
	case "long":
		rc.Long = true
	}
	if rc.Input == nil {
		rc.Input = registry.EmptyObjectSchema()
	}
	rc.Handler = func(ctx context.Context, in json.RawMessage) (any, error) {
		cl := &call{Input: in, Progress: func(p envspec.Progress) {
			registry.Progress(ctx, registry.Update{
				Stage: p.Stage, Message: p.Message,
				Done: p.Done, Total: p.Total, Unit: p.Unit,
				Data: p,
			})
		}}
		out, err := c.Handler(ctx, cl)
		if err != nil {
			return nil, registryErr(err)
		}
		return out, nil
	}
	return rc
}

func decode[T any](call *call, v *T) error {
	if len(call.Input) == 0 {
		return nil
	}
	if err := json.Unmarshal(call.Input, v); err != nil {
		return &agentError{Code: "invalid", Message: "bad input: " + err.Error()}
	}
	return nil
}

func (s *set) owner(ctx context.Context) (envspec.Owner, error) {
	if s.deps.Owner == nil {
		return envspec.Owner{}, &agentError{Code: "error", Message: "no owner resolver configured"}
	}
	return s.deps.Owner(ctx)
}

// jobID is the registry job running this call, strictly from
// registry.JobIDFrom — never owner.Job, which names a different
// execution. Outside a registry job it is "".
func (s *set) jobID(ctx context.Context) string {
	id, _ := registry.JobIDFrom(ctx)
	return id
}

// borrowJob is the execution a lease borrow is taken for: the
// registry job when there is one, else the owner resolver's job
// (hosts invoking capabilities synchronously still fence borrows).
func (s *set) borrowJob(ctx context.Context, owner envspec.Owner) string {
	if id := s.jobID(ctx); id != "" {
		return id
	}
	return owner.Job
}

// operation builds the artifact-run identity for one handler call.
// ID is always set: a lifecycle handler that already started its
// durable operation passes it as durableID; other runs mint a fresh
// publication id here.
func (s *set) operation(ctx context.Context, capID, durableID, envID string, inputs []string) Operation {
	op := Operation{
		ID:         durableID,
		Capability: capID,
		Parent:     parentFrom(ctx),
		Env:        envID,
		Inputs:     inputs,
	}
	if op.ID == "" {
		op.ID = envspec.NewID("op_")
	}
	op.JobID = s.jobID(ctx)
	if envID != "" && s.deps.Manager != nil {
		if ident, err := s.deps.Manager.EnvIdentity(envID); err == nil {
			op.Generation = ident.Generation
			op.LeaseEpoch = ident.LeaseEpoch
		}
	}
	return op
}

// envOwned resolves an environment the caller may act on: theirs by
// owner id or by session. There is no cross-owner borrow override —
// foreign environments are deliberately indistinguishable from gone.
func (s *set) envOwned(ctx context.Context, id string) (envspec.Info, envspec.Owner, error) {
	owner, err := s.owner(ctx)
	if err != nil {
		return envspec.Info{}, owner, err
	}
	info, err := s.deps.Manager.EnvInfo(id)
	if err != nil {
		return envspec.Info{}, owner, err
	}
	sameOwner := owner.ID != "" && info.Owner.ID == owner.ID
	sameSession := owner.Session != "" && info.Owner.Session == owner.Session
	if !sameOwner && !sameSession {
		return envspec.Info{}, owner, &agentError{Code: "not_found",
			Message: fmt.Sprintf("environment %s is not among yours", id),
			Hint:    "env.status lists what you hold; env.provision makes a new one"}
	}
	return info, owner, nil
}

// stampLifecycle adds the distinct identities of a lifecycle result:
// the durable operation, the registry job, the environment and its
// generation/lease epoch are four different things and read so.
func (s *set) stampLifecycle(ctx context.Context, out map[string]any, res EnvResult) map[string]any {
	out["operation_id"] = res.OperationID
	out["generation"] = res.Generation
	out["lease_epoch"] = res.LeaseEpoch
	if job := s.jobID(ctx); job != "" {
		out["job_id"] = job
	}
	return out
}

func requireManager(s *set) error {
	if s.deps.Manager == nil {
		return &agentError{Code: "unavailable", Message: "environments are not configured on this host"}
	}
	return nil
}

func shortDigest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:12]
}

// agentEnvInfo is the trimmed, plumbing-free environment view.
type agentEnvInfo struct {
	Env        string                `json:"env"`
	Kind       string                `json:"kind"`
	Profile    string                `json:"profile,omitempty"`
	State      string                `json:"state"`
	Workspace  string                `json:"workspace,omitempty"`
	AgeSeconds float64               `json:"age_seconds,omitempty"`
	TTLSeconds float64               `json:"ttl_seconds,omitempty"`
	Image      string                `json:"image,omitempty"`
	Endpoints  map[string]string     `json:"endpoints,omitempty"`
	Components []componentView       `json:"components,omitempty"`
	Services   []envspec.ServiceInfo `json:"services,omitempty"`
}

type componentView struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Repairs int    `json:"repairs,omitempty"`
}

func viewInfo(info envspec.Info) agentEnvInfo {
	v := agentEnvInfo{Env: info.ID, Kind: string(info.Kind), Profile: info.Profile,
		State: string(info.State), Workspace: info.Workspace, Image: info.Image, Endpoints: info.Endpoints, Services: info.Services}
	if !info.Created.IsZero() {
		v.AgeSeconds = time.Since(info.Created).Seconds()
	}
	if info.TTLLeft > 0 {
		v.TTLSeconds = info.TTLLeft.Seconds()
	}
	for _, c := range info.Components {
		v.Components = append(v.Components, componentView{Name: c.Name, State: c.State,
			Detail: c.LastFailure, Repairs: c.Repairs})
	}
	return v
}

// inputSchema derives a capability's input schema from its Go input
// type, so schema and decoding cannot drift. Structs become strict
// objects; json.RawMessage fields accept any JSON value. It panics
// on a type that cannot be described: a programmer error the
// package tests catch.
func inputSchema[T any]() *jsonschema.Schema {
	sch, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: map[reflect.Type]*jsonschema.Schema{
		reflect.TypeFor[json.RawMessage](): {},
	}})
	if err != nil {
		panic(fmt.Sprintf("envcap: input schema for %v: %v", reflect.TypeFor[T](), err))
	}
	return sch
}
