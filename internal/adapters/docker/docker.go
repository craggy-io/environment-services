// Package docker is the container adapter: the Docker Engine API
// through the official Go client, over the Windows named pipe or a
// Unix socket. Podman's Docker-compatible socket also works; specs
// never name either runtime. Defaults suit sample handling: per-env
// internal networks, --cap-drop ALL, no-new-privileges, pids limit,
// no host mounts.
//
// Identity is new-only: containers and networks are named from the
// deterministic env name (adapter.NameFor: craggy-<envID>) and
// labelled craggy.env / craggy.manager / craggy.profile /
// craggy.sidecar. Discover is scoped to this manager's
// craggy.manager label; craggy.env containers under another manager
// are reported Foreign, never adopted. Nothing else is recognised.
//
// Presence mapping is explicit: a definitive Inspect 404 is Absent;
// a backend that cannot be reached (or any other Inspect failure)
// is Unknown — never invented absence. Engine connection failures
// elsewhere wrap adapter.ErrBackendDown.
package docker

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// Labels carried by every resource this adapter creates.
const (
	LabelEnv     = "craggy.env"
	LabelManager = "craggy.manager"
	LabelProfile = "craggy.profile"
	LabelSidecar = "craggy.sidecar"
	// labelSpecPrefix carries spec labels without colliding with the
	// identity labels above.
	labelSpecPrefix = "craggy.label."
)

// engine is the slice of the Engine API this adapter uses. The real
// *client.Client satisfies it; tests substitute a fake. It never
// leaves this package — collectors receive it as a typed port from
// their own adapter.
type engine interface {
	Ping(ctx context.Context, options client.PingOptions) (client.PingResult, error)
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerAttach(ctx context.Context, containerID string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerLogs(ctx context.Context, containerID string, options client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerStats(ctx context.Context, containerID string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error)
	NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkRemove(ctx context.Context, networkID string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	ExecCreate(ctx context.Context, containerID string, options client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(ctx context.Context, execID string, options client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(ctx context.Context, execID string, options client.ExecInspectOptions) (client.ExecInspectResult, error)
	CopyToContainer(ctx context.Context, containerID string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	CopyFromContainer(ctx context.Context, containerID string, options client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImageBuild(ctx context.Context, buildContext io.Reader, options client.ImageBuildOptions) (client.ImageBuildResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
}

// Adapter is the Docker container adapter.
type Adapter struct {
	cli       engine
	ManagerID string // ownership label value (craggy.manager)
	// SidecarImage is the egress proxy image for NetworkEgress.
	SidecarImage string
	// Start starts the runtime for StartBackend; nil when it cannot
	// be started from here.
	Start func(context.Context) error
	// BootTimeout bounds StartBackend (default bootTimeout).
	BootTimeout time.Duration
}

// New builds an adapter from the default client (DOCKER_HOST aware).
func New(managerID string) (*Adapter, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Adapter{cli: cli, ManagerID: managerID, SidecarImage: "mitmproxy/mitmproxy:latest", Start: localStarter()}, nil
}

func (a *Adapter) Describe() adapter.Description {
	return adapter.Description{
		Name:         "docker",
		Kinds:        []envspec.Kind{envspec.Container},
		Relationship: adapter.Created,
		Version:      "1",
	}
}

// Check pings the daemon. It never starts it (StartBackend does).
func (a *Adapter) Check(ctx context.Context) error {
	_, err := a.cli.Ping(ctx, client.PingOptions{})
	return engineErr(err)
}

// engineErr marks an Engine API error that means the daemon could
// not be reached as adapter.ErrBackendDown, so the core can tell a
// dead daemon from a failed operation without reading message text.
func engineErr(err error) error {
	if err != nil && client.IsErrConnectionFailed(err) {
		return adapter.BackendDown(err)
	}
	return err
}

// isNotFound reports a definitive Engine 404 — the one Inspect
// failure that means Absent rather than Unknown.
func isNotFound(err error) bool { return err != nil && cerrdefs.IsNotFound(err) }

// Capacity returns the Docker VM's MemTotal (MB) and NCPU (admission pool).
func (a *Adapter) Capacity(ctx context.Context) (memMB int64, cpus int, err error) {
	res, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return 0, 0, engineErr(err)
	}
	return res.Info.MemTotal / (1 << 20), res.Info.NCPU, nil
}

func (a *Adapter) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	if strings.EqualFold(s.Params["container.privileged"], "true") {
		return fmt.Errorf("%w: privileged containers are never granted; remove container.privileged (sidecars get extra capabilities, targets never do)",
			envspec.ErrInvalid)
	}
	return nil
}

func (a *Adapter) Cost(s envspec.Spec) adapter.Cost {
	return adapter.Cost{Pool: envspec.PoolDocker, CPUs: s.Resources.CPUs, MemoryMB: s.Resources.MemoryMB, DiskMB: s.Resources.DiskMB}
}

// shortID is the part of an environment id image names are built
// from. Ids are time-ordered, so a plain prefix is only a timestamp
// (the first 12 characters of a UUIDv7 are about 16 ms wide) and
// two environments created in the same instant would collide. It
// joins the start of the id, which orders by creation time, to its
// random tail, which tells apart environments created together.
// Container/network names use the full deterministic NameFor.
func shortID(id string) string {
	s := strings.TrimPrefix(id, "env_")
	if len(s) <= 20 {
		return s
	}
	return s[:8] + "-" + s[len(s)-8:]
}

func networkNameFor(envID string) string { return adapter.NameFor(envID) + "-net" }

func (a *Adapter) labelsFor(envID string, s envspec.Spec) map[string]string {
	labels := map[string]string{LabelEnv: envID, LabelManager: a.ManagerID, LabelProfile: s.Profile}
	for k, v := range s.Labels {
		labels[labelSpecPrefix+k] = v
	}
	return labels
}

// --- Observe / Discover ---

// Observe inspects the ref's container (by recorded ID, else by
// deterministic name). Inspect 404 is Absent; any other failure is
// Unknown with Detail — backend-down included.
func (a *Adapter) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	obs := adapter.Observation{Ref: ref, Workspace: "/work"}
	if obs.Ref.EnvID == "" {
		obs.Ref.EnvID = op.EnvID
	}
	if obs.Ref.Name == "" && obs.Ref.EnvID != "" {
		obs.Ref.Name = adapter.NameFor(obs.Ref.EnvID)
	}
	target := ref.External
	if target == "" {
		target = obs.Ref.Name
	}
	if target == "" {
		obs.Presence, obs.Detail = adapter.Unknown, "no container id or name in ref"
		return obs, nil
	}
	insp, err := a.cli.ContainerInspect(ctx, target, client.ContainerInspectOptions{})
	if err != nil {
		if isNotFound(err) {
			obs.Presence = adapter.Absent
			return obs, nil
		}
		obs.Presence = adapter.Unknown
		if client.IsErrConnectionFailed(err) {
			obs.Detail = fmt.Sprintf("%s: %v", adapter.ErrBackendDown, err)
		} else {
			obs.Detail = fmt.Sprintf("inspect %s: %v", target, err)
		}
		return obs, nil
	}
	c := insp.Container
	obs.Ref.External = c.ID
	obs.Ref.Name = strings.TrimPrefix(c.Name, "/")
	if c.Config != nil {
		if envID := c.Config.Labels[LabelEnv]; envID != "" {
			obs.Ref.EnvID = envID
		}
	}
	obs.Image = a.digestOfInspect(ctx, c.Image)
	obs.Endpoints = map[string]string{"container": c.ID, "name": obs.Ref.Name}
	if c.State != nil {
		obs.Detail = string(c.State.Status)
	}
	obs.Presence = adapter.Present
	return obs, nil
}

// Discover lists containers in scope: ours (craggy.manager == this
// manager, sidecars excluded — they are owned by their target) and
// Foreign craggy.env containers of other managers.
func (a *Adapter) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	mgr := scope.ManagerID
	if mgr == "" {
		mgr = a.ManagerID
	}
	res, err := a.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, engineErr(err)
	}
	var out []adapter.Candidate
	for _, sum := range res.Items {
		envID := sum.Labels[LabelEnv]
		if envID == "" || sum.Labels[LabelSidecar] != "" {
			continue
		}
		if scope.EnvID != "" && envID != scope.EnvID && sum.Labels[LabelManager] == mgr {
			continue
		}
		name := ""
		if len(sum.Names) > 0 {
			name = strings.TrimPrefix(sum.Names[0], "/")
		}
		cand := adapter.Candidate{Ref: adapter.Ref{EnvID: envID, External: sum.ID, Name: name}}
		if sum.Labels[LabelManager] != mgr {
			cand.Foreign = true
			cand.Detail = fmt.Sprintf("%s=%s", LabelManager, sum.Labels[LabelManager])
			if scope.EnvID != "" && envID != scope.EnvID {
				continue
			}
		}
		out = append(out, cand)
	}
	return out, nil
}

func (a *Adapter) digestOfInspect(ctx context.Context, ref string) string {
	digest, _, _ := a.imageDigest(ctx, ref)
	if digest == "" {
		return ref
	}
	return digest
}

// inspect builds an Instance from a live inspect of target (id or name).
func (a *Adapter) inspect(ctx context.Context, target string) (*Instance, error) {
	insp, err := a.cli.ContainerInspect(ctx, target, client.ContainerInspectOptions{})
	if err != nil {
		return nil, engineErr(err)
	}
	c := insp.Container
	inst := &Instance{ad: a, id: c.ID, name: strings.TrimPrefix(c.Name, "/"),
		image: a.digestOfInspect(ctx, c.Image), credEnv: map[string]string{}}
	if c.Config != nil {
		inst.envID = c.Config.Labels[LabelEnv]
		inst.spec.Profile = c.Config.Labels[LabelProfile]
		inst.spec.Kind = envspec.Container
		inst.spec.Labels = map[string]string{}
		for k, v := range c.Config.Labels {
			if strings.HasPrefix(k, labelSpecPrefix) {
				inst.spec.Labels[strings.TrimPrefix(k, labelSpecPrefix)] = v
			}
		}
		if len(inst.spec.Labels) == 0 {
			inst.spec.Labels = nil
		}
	}
	if c.HostConfig != nil {
		inst.network = string(c.HostConfig.NetworkMode)
	}
	return inst, nil
}

// Open re-materialises an instance from a ref after restart.
func (a *Adapter) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Instance, error) {
	target := ref.External
	if target == "" {
		target = ref.Name
	}
	if target == "" && ref.EnvID != "" {
		target = adapter.NameFor(ref.EnvID)
	}
	if target == "" {
		target = adapter.NameFor(op.EnvID)
	}
	inst, err := a.inspect(ctx, target)
	if err != nil {
		return nil, err
	}
	if inst.envID == "" {
		inst.envID = ref.EnvID
	}
	if inst.envID == "" {
		inst.envID = op.EnvID
	}
	return inst, nil
}
