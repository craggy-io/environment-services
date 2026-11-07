package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Instance is one container.
type Instance struct {
	ad      *Adapter
	envID   string
	id      string // container ID
	name    string
	network string
	spec    envspec.Spec
	image   string // immutable digest

	mu      sync.Mutex
	credEnv map[string]string
}

func (i *Instance) Ref() adapter.Ref {
	return adapter.Ref{EnvID: i.envID, External: i.id, Name: i.name}
}

// ContainerID exposes the container ID to this adapter's collectors.
func (i *Instance) ContainerID() string { return i.id }

// ImageDigest reports the immutable image identity the core records
// at tx2 (its optional interface{ ImageDigest() string } assertion).
func (i *Instance) ImageDigest() string { return i.image }

// Create pulls on demand (progress-reported), makes the per-env
// network, creates and starts the container under its deterministic
// name with a keepalive command. A retry with the same env ID finds
// the container by that name instead of creating a second one.
func (a *Adapter) Create(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	id := op.EnvID
	if id == "" {
		return nil, fmt.Errorf("%w: docker create needs an env id", envspec.ErrInvalid)
	}
	name := adapter.NameFor(id)
	// Rediscovery: an earlier attempt may have created (and started)
	// this env's container before a crash.
	if existing, err := a.inspect(ctx, name); err == nil {
		existing.envID = id
		existing.spec = s
		if insp, ierr := a.cli.ContainerInspect(ctx, existing.id, client.ContainerInspectOptions{}); ierr == nil &&
			(insp.Container.State == nil || !insp.Container.State.Running) {
			if _, serr := a.cli.ContainerStart(ctx, existing.id, client.ContainerStartOptions{}); serr != nil {
				return existing, fmt.Errorf("docker: restart rediscovered container: %w", engineErr(serr))
			}
		}
		return existing, nil
	} else if !isNotFound(err) && !client.IsErrConnectionFailed(err) {
		// A definitive 404 falls through to create; connection
		// failure surfaces on the create path below.
		_ = err
	}

	report := func(stage, msg string, done, total int64, unit string) {
		if progress != nil {
			progress(envspec.Progress{Op: "provision", Stage: stage, EnvID: id, Done: done, Total: total, Unit: unit, Message: msg})
		}
	}
	image := s.Image
	if image == "" {
		return nil, fmt.Errorf("docker: spec has no image (profile %q)", s.Profile)
	}
	digest, exists, _ := a.imageDigest(ctx, image)
	if !exists {
		report("pull-image", "pulling "+image, 0, 0, "bytes")
		if err := a.pull(ctx, image, progress, id); err != nil {
			return nil, fmt.Errorf("docker: pull %s: %w", image, engineErr(err))
		}
		digest, _, _ = a.imageDigest(ctx, image)
	}

	// Per-environment network.
	netName := networkNameFor(id)
	netMode := container.NetworkMode("none")
	mkNet := func(internal bool) error {
		_, err := a.cli.NetworkCreate(ctx, netName, client.NetworkCreateOptions{
			Driver: "bridge", Internal: internal,
			Labels: map[string]string{LabelEnv: id, LabelManager: a.ManagerID},
		})
		if err == nil {
			netMode = container.NetworkMode(netName)
		}
		return err
	}
	switch s.Network {
	case envspec.NetworkNone, "":
		_ = mkNet(true) // best-effort, as before: "none" still isolates
	case envspec.NetworkIsolated:
		if err := mkNet(true); err != nil {
			return nil, fmt.Errorf("docker: create network: %w", engineErr(err))
		}
	case envspec.NetworkEgress:
		if err := mkNet(false); err != nil {
			return nil, fmt.Errorf("docker: create network: %w", engineErr(err))
		}
	case envspec.NetworkOpen:
		netMode = container.NetworkMode("bridge")
	}

	report("create", "creating container", 0, 0, "")
	user := s.Params["container.user"]
	mem := s.Resources.MemoryMB << 20
	var cpuQuota, cpuPeriod int64
	if s.Resources.CPUs > 0 {
		cpuPeriod = 100000
		cpuQuota = int64(s.Resources.CPUs * 100000)
	}
	pids := int64(512)
	create, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: image, User: user, Labels: a.labelsFor(id, s),
			Cmd:          []string{"sleep", "infinity"},
			WorkingDir:   "/work",
			Tty:          false,
			ExposedPorts: publishedPortSet(s.PublishedTCPPort),
		},
		HostConfig: &container.HostConfig{
			NetworkMode:  netMode,
			PortBindings: publishedPortBindings(s.PublishedTCPPort),
			CapDrop:      []string{"ALL"},
			SecurityOpt:  []string{"no-new-privileges"},
			Resources: container.Resources{
				Memory:     mem,
				MemorySwap: mem,
				CPUQuota:   cpuQuota,
				CPUPeriod:  cpuPeriod,
				PidsLimit:  &pids,
			},
			Tmpfs: map[string]string{"/run/secrets": "rw,noexec,nosuid,size=16m,mode=755"},
		},
	})
	if err != nil {
		a.removeNetwork(ctx, netName)
		return nil, fmt.Errorf("docker: create container: %w", engineErr(err))
	}
	inst := &Instance{ad: a, envID: id, id: create.ID, name: name, network: netName,
		spec: s, image: digest, credEnv: map[string]string{}}
	if _, err := a.cli.ContainerStart(ctx, create.ID, client.ContainerStartOptions{}); err != nil {
		// Partial success: hand the instance back with the error so
		// the core records it and the release op cleans up.
		return inst, fmt.Errorf("docker: start container: %w", engineErr(err))
	}

	// Some minimal images have no /work; ensure the workspace exists.
	if p, err := inst.Exec(ctx, op, adapter.ExecCmd{Argv: []string{"mkdir", "-p", "/work"}}); err == nil {
		drainWait(p)
	}

	// Egress sidecar (mitmproxy) joins the env network as the
	// capture proxy; it shares nothing with the target's caps.
	if s.Network == envspec.NetworkEgress && s.Params["proxy.sidecar"] != "false" {
		if err := a.startEgressSidecar(ctx, inst, netName); err != nil {
			// Non-fatal: egress still works via NAT; capture just lacks http.
			_ = err
		}
	}
	return inst, nil
}

func publishedPort(s uint16) network.Port {
	return network.MustParsePort(strconv.Itoa(int(s)) + "/tcp")
}

func publishedPortSet(s uint16) network.PortSet {
	if s == 0 {
		return nil
	}
	return network.PortSet{publishedPort(s): {}}
}

func publishedPortBindings(s uint16) network.PortMap {
	if s == 0 {
		return nil
	}
	return network.PortMap{publishedPort(s): {{HostIP: netip.MustParseAddr("127.0.0.1")}}}
}

func (a *Adapter) startEgressSidecar(ctx context.Context, inst *Instance, netName string) error {
	img := a.SidecarImage
	if _, exists, _ := a.imageDigest(ctx, img); !exists {
		if err := a.pull(ctx, img, nil, inst.envID); err != nil {
			return err
		}
	}
	res, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: inst.name + "-proxy",
		Config: &container.Config{
			Image:  img,
			Cmd:    []string{"mitmdump", "--listen-port", "8080", "--set", "block_global=false"},
			Labels: map[string]string{LabelEnv: inst.envID, LabelManager: a.ManagerID, LabelProfile: inst.spec.Profile, LabelSidecar: "proxy"},
		},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(netName)},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{netName: {Aliases: []string{"proxy"}}},
		},
	})
	if err != nil {
		return err
	}
	_, err = a.cli.ContainerStart(ctx, res.ID, client.ContainerStartOptions{})
	return err
}

func (a *Adapter) removeNetwork(ctx context.Context, name string) {
	if name == "" {
		return
	}
	_, _ = a.cli.NetworkRemove(ctx, name, client.NetworkRemoveOptions{})
}

// Dispose removes sidecars, the container and its network.
// Disconnect is not meaningful for created containers and is
// rejected; the core only sends Destroy here.
func (i *Instance) Dispose(ctx context.Context, op adapter.OpContext, disp adapter.Disposition) error {
	if disp != adapter.Destroy && disp != adapter.ResetToPool {
		return fmt.Errorf("%w: docker disposition %q", adapter.ErrUnsupported, disp)
	}
	if res, err := i.ad.cli.ContainerList(ctx, client.ContainerListOptions{All: true}); err == nil {
		for _, sum := range res.Items {
			if sum.Labels[LabelEnv] == i.envID && sum.ID != i.id {
				_, _ = i.ad.cli.ContainerRemove(ctx, sum.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
			}
		}
	}
	_, err := i.ad.cli.ContainerRemove(ctx, i.id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && isNotFound(err) {
		err = nil // already gone: disposal achieved
	}
	i.ad.removeNetwork(ctx, i.network)
	if err != nil {
		return engineErr(err)
	}
	return nil
}

// --- ImageBuilder ---

func (a *Adapter) imageDigest(ctx context.Context, ref string) (string, bool, error) {
	res, err := a.cli.ImageInspect(ctx, ref)
	if err != nil {
		return "", false, engineErr(err)
	}
	insp := res.InspectResponse
	if insp.ID != "" {
		return insp.ID, true, nil
	}
	if len(insp.RepoDigests) > 0 {
		return insp.RepoDigests[0], true, nil
	}
	return insp.ID, insp.ID != "", nil
}

// ImageExists implements adapter.ImageBuilder.
func (a *Adapter) ImageExists(ctx context.Context, ref string) (string, bool, error) {
	return a.imageDigest(ctx, ref)
}

// imageTagFor is the content-tagged image ref for a Build profile.
// It must match the core's derivation (adapters cannot import core).
func imageTagFor(name, digest string) string {
	clean := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return "craggy/" + clean + ":" + digest
}

// EnsureImage builds (content-tagged, no temp dir) or pulls a
// profile image, streaming progress.
func (a *Adapter) EnsureImage(ctx context.Context, p envspec.Profile, digest string, progress func(envspec.Progress)) (string, string, error) {
	report := func(stage, msg string, done, total int64) {
		if progress != nil {
			progress(envspec.Progress{Op: "build", Stage: stage, Done: done, Total: total, Message: msg})
		}
	}
	if p.Build == nil {
		ref := p.Image
		id, exists, _ := a.imageDigest(ctx, ref)
		if exists {
			return ref, id, nil
		}
		if err := a.pull(ctx, ref, progress, ""); err != nil {
			return "", "", engineErr(err)
		}
		id, _, err := a.imageDigest(ctx, ref)
		return ref, id, err
	}
	ref := imageTagFor(p.Name, digest)
	if id, exists, _ := a.imageDigest(ctx, ref); exists {
		return ref, id, nil // same content builds once
	}
	var ctxBuf bytes.Buffer
	src, err := contextSource(p.Build.Context)
	if err != nil {
		return "", "", err
	}
	if _, err := xfer.WriteTar(ctx, src, &ctxBuf, xfer.Options{}); err != nil {
		return "", "", fmt.Errorf("docker: tar build context: %w", err)
	}
	args := map[string]*string{}
	for k, v := range p.Build.Args {
		vv := v
		args[k] = &vv
	}
	dockerfile := p.Build.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	res, err := a.cli.ImageBuild(ctx, &ctxBuf, client.ImageBuildOptions{
		Tags: []string{ref}, Dockerfile: dockerfile, BuildArgs: args,
		Remove: true, PullParent: false,
	})
	if err != nil {
		return "", "", fmt.Errorf("docker: build %s: %w", p.Name, engineErr(err))
	}
	defer res.Body.Close()
	step := int64(0)
	dec := json.NewDecoder(res.Body)
	for {
		var msg buildMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return "", "", err
		}
		if msg.Error != nil {
			return "", "", fmt.Errorf("docker: build %s: %s", p.Name, msg.Error.Message)
		}
		if line := strings.TrimSpace(msg.Stream); line != "" {
			step++
			report("build", line, step, 0)
		} else if msg.Status != "" {
			report("build", msg.Status, 0, 0)
		}
	}
	id, _, err := a.imageDigest(ctx, ref)
	return ref, id, err
}

func (a *Adapter) pull(ctx context.Context, ref string, progress func(envspec.Progress), envID string) error {
	resp, err := a.cli.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer resp.Close()
	dec := json.NewDecoder(resp)
	for {
		var msg buildMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if msg.Error != nil {
			return fmt.Errorf("%s", msg.Error.Message)
		}
		if msg.Progress != nil && progress != nil {
			progress(envspec.Progress{Op: "provision", Stage: "pull-image", EnvID: envID,
				Done: msg.Progress.Current, Total: msg.Progress.Total, Unit: "bytes",
				Message: msg.Status + " " + msg.ID})
		}
	}
	return nil
}

// buildMessage is one line of the build/pull JSON stream.
type buildMessage struct {
	Stream   string `json:"stream"`
	Status   string `json:"status"`
	ID       string `json:"id"`
	Progress *struct {
		Current int64 `json:"current"`
		Total   int64 `json:"total"`
	} `json:"progressDetail"`
	Error *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

// contextSource adapts a build-context fs.FS to an envspec.Source.
func contextSource(fsys fs.FS) (envspec.Source, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("docker: read build context: %w", err)
	}
	return envspec.MemSource(files), nil
}

func drainWait(p *adapter.Process) (int, error) {
	var wg sync.WaitGroup
	for _, r := range []io.Reader{p.Stdout, p.Stderr} {
		if r == nil {
			continue
		}
		wg.Add(1)
		go func(r io.Reader) { defer wg.Done(); _, _ = io.Copy(io.Discard, r) }(r)
	}
	code, err := p.Wait()
	wg.Wait()
	return code, err
}
