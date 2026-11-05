package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/internal/xfer"
)

// fakeEngine is an in-memory Engine API for adapter tests: enough
// container/network/image/exec behaviour to exercise identity,
// presence mapping and lifecycle without a daemon.
type fakeEngine struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer // by ID
	byName     map[string]string
	networks   map[string]bool
	images     map[string]string // ref -> id
	execs      map[string]*fakeExec
	seq        int

	// knobs
	down       bool // every call fails like a dead daemon (generic error)
	inspectErr error
	removeErr  error
	startErr   error

	creates int
	removes int
}

type fakeContainer struct {
	id      string
	name    string
	cfg     *container.Config
	hostCfg *container.HostConfig
	running bool
	files   map[string][]byte // flat path -> content (archive tests)
	logBuf  []byte            // framed log stream
}

type fakeExec struct {
	id     string
	cid    string
	cmd    []string
	user   string
	stdout []byte
	stderr []byte
	code   int
	done   bool
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		containers: map[string]*fakeContainer{},
		byName:     map[string]string{},
		networks:   map[string]bool{},
		images:     map[string]string{"busybox:latest": "sha256:busyboxid"},
		execs:      map[string]*fakeExec{},
	}
}

func (f *fakeEngine) err() error {
	if f.down {
		return fmt.Errorf("fake: daemon unreachable")
	}
	return nil
}

func notFound(what string) error { return fmt.Errorf("%w: %s", cerrdefs.ErrNotFound, what) }

func (f *fakeEngine) nextID(prefix string) string {
	f.seq++
	return fmt.Sprintf("%s%06d", prefix, f.seq)
}

func (f *fakeEngine) Ping(ctx context.Context, o client.PingOptions) (client.PingResult, error) {
	return client.PingResult{}, f.err()
}

func (f *fakeEngine) Info(ctx context.Context, o client.InfoOptions) (client.SystemInfoResult, error) {
	if err := f.err(); err != nil {
		return client.SystemInfoResult{}, err
	}
	return client.SystemInfoResult{Info: system.Info{MemTotal: 8 << 30, NCPU: 4}}, nil
}

func (f *fakeEngine) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := f.err(); err != nil {
		return client.ContainerCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if o.Name != "" {
		if _, dup := f.byName[o.Name]; dup {
			return client.ContainerCreateResult{}, fmt.Errorf("%w: container name %q already in use", cerrdefs.ErrConflict, o.Name)
		}
	}
	id := f.nextID("cid")
	fc := &fakeContainer{id: id, name: o.Name, cfg: o.Config, hostCfg: o.HostConfig, files: map[string][]byte{}}
	f.containers[id] = fc
	if o.Name != "" {
		f.byName[o.Name] = id
	}
	f.creates++
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeEngine) find(target string) (*fakeContainer, error) {
	if c, ok := f.containers[target]; ok {
		return c, nil
	}
	if id, ok := f.byName[strings.TrimPrefix(target, "/")]; ok {
		return f.containers[id], nil
	}
	return nil, notFound("container " + target)
}

func (f *fakeEngine) ContainerStart(ctx context.Context, id string, o client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if err := f.err(); err != nil {
		return client.ContainerStartResult{}, err
	}
	if f.startErr != nil {
		return client.ContainerStartResult{}, f.startErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return client.ContainerStartResult{}, err
	}
	c.running = true
	return client.ContainerStartResult{}, nil
}

func (f *fakeEngine) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := f.err(); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	if f.removeErr != nil {
		return client.ContainerRemoveResult{}, f.removeErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return client.ContainerRemoveResult{}, err
	}
	delete(f.containers, c.id)
	delete(f.byName, c.name)
	f.removes++
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerList(ctx context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	if err := f.err(); err != nil {
		return client.ContainerListResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var items []container.Summary
	for _, c := range f.containers {
		var labels map[string]string
		img := ""
		if c.cfg != nil {
			labels, img = c.cfg.Labels, c.cfg.Image
		}
		items = append(items, container.Summary{ID: c.id, Names: []string{"/" + c.name}, Labels: labels, Image: img})
	}
	return client.ContainerListResult{Items: items}, nil
}

func (f *fakeEngine) ContainerInspect(ctx context.Context, target string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if f.inspectErr != nil {
		return client.ContainerInspectResult{}, f.inspectErr
	}
	if err := f.err(); err != nil {
		return client.ContainerInspectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(target)
	if err != nil {
		return client.ContainerInspectResult{}, err
	}
	status := container.ContainerState("exited")
	if c.running {
		status = container.ContainerState("running")
	}
	imgRef := ""
	if c.cfg != nil {
		imgRef = c.cfg.Image
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID: c.id, Name: "/" + c.name, Image: imgRef, Config: c.cfg, HostConfig: c.hostCfg,
		State: &container.State{Status: status, Running: c.running},
	}}, nil
}

func frame(stream byte, payload string) []byte {
	hdr := make([]byte, 8)
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(payload)))
	return append(hdr, payload...)
}

func (f *fakeEngine) ContainerAttach(ctx context.Context, id string, o client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	return client.ContainerAttachResult{}, fmt.Errorf("fake: attach unsupported")
}

type logsResult struct{ io.ReadCloser }

func (f *fakeEngine) ContainerLogs(ctx context.Context, id string, o client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(id)
	if err != nil {
		return nil, err
	}
	return logsResult{io.NopCloser(bytes.NewReader(c.logBuf))}, nil
}

func (f *fakeEngine) ContainerStats(ctx context.Context, id string, o client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	if err := f.err(); err != nil {
		return client.ContainerStatsResult{}, err
	}
	st := container.StatsResponse{}
	st.PidsStats.Current = 3
	st.MemoryStats.Usage = 50 << 20
	st.MemoryStats.Limit = 512 << 20
	st.CPUStats.CPUUsage.TotalUsage = 200
	st.CPUStats.SystemUsage = 1000
	st.PreCPUStats.CPUUsage.TotalUsage = 100
	st.PreCPUStats.SystemUsage = 500
	b, _ := json.Marshal(st)
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(b))}, nil
}

func (f *fakeEngine) NetworkCreate(ctx context.Context, name string, o client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if err := f.err(); err != nil {
		return client.NetworkCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = true
	return client.NetworkCreateResult{ID: "net-" + name}, nil
}

func (f *fakeEngine) NetworkRemove(ctx context.Context, id string, o client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, id)
	return client.NetworkRemoveResult{}, f.err()
}

// fake exec semantics: mkdir/chmod succeed silently; `echo X` (via
// sh -c too) writes X to stdout; `cat path` reads fake files.
func (f *fakeEngine) ExecCreate(ctx context.Context, cid string, o client.ExecCreateOptions) (client.ExecCreateResult, error) {
	if err := f.err(); err != nil {
		return client.ExecCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(cid)
	if err != nil {
		return client.ExecCreateResult{}, err
	}
	ex := &fakeExec{id: f.nextID("exec"), cid: c.id, cmd: o.Cmd, user: o.User, done: true}
	argv := o.Cmd
	if len(argv) >= 3 && argv[0] == "sh" && argv[1] == "-c" {
		argv = strings.Fields(argv[2])
	}
	if len(argv) > 0 {
		switch argv[0] {
		case "mkdir", "chmod":
		case "echo":
			ex.stdout = []byte(strings.Join(argv[1:], " ") + "\n")
		case "cat":
			if len(argv) > 1 {
				if b, ok := c.files[argv[1]]; ok {
					ex.stdout = b
				} else {
					ex.stderr = []byte("cat: no such file\n")
					ex.code = 1
				}
			}
		case "false":
			ex.code = 1
		}
	}
	f.execs[ex.id] = ex
	return client.ExecCreateResult{ID: ex.id}, nil
}

func (f *fakeEngine) ExecAttach(ctx context.Context, execID string, o client.ExecAttachOptions) (client.ExecAttachResult, error) {
	if err := f.err(); err != nil {
		return client.ExecAttachResult{}, err
	}
	f.mu.Lock()
	ex, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return client.ExecAttachResult{}, notFound("exec " + execID)
	}
	a, b := net.Pipe()
	go func() {
		if len(ex.stdout) > 0 {
			_, _ = b.Write(frame(1, string(ex.stdout)))
		}
		if len(ex.stderr) > 0 {
			_, _ = b.Write(frame(2, string(ex.stderr)))
		}
		_ = b.Close()
	}()
	return client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(a, "")}, nil
}

func (f *fakeEngine) ExecInspect(ctx context.Context, execID string, o client.ExecInspectOptions) (client.ExecInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ex, ok := f.execs[execID]
	if !ok {
		return client.ExecInspectResult{}, notFound("exec " + execID)
	}
	return client.ExecInspectResult{ID: ex.id, ContainerID: ex.cid, Running: false, ExitCode: ex.code}, nil
}

type fakeSink struct {
	f    *fakeEngine
	c    *fakeContainer
	dest string
}

func (s *fakeSink) Add(path string, mode int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if mode&0o170000 == 0o040000 {
		return nil
	}
	s.f.mu.Lock()
	s.c.files[strings.TrimSuffix(s.dest, "/")+"/"+path] = b
	s.f.mu.Unlock()
	return nil
}

func (f *fakeEngine) CopyToContainer(ctx context.Context, cid string, o client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if err := f.err(); err != nil {
		return client.CopyToContainerResult{}, err
	}
	f.mu.Lock()
	c, err := f.find(cid)
	f.mu.Unlock()
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	_, err = xfer.ReadTar(ctx, o.Content, &fakeSink{f: f, c: c, dest: o.DestinationPath}, xfer.Options{})
	return client.CopyToContainerResult{}, err
}

func (f *fakeEngine) CopyFromContainer(ctx context.Context, cid string, o client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	if err := f.err(); err != nil {
		return client.CopyFromContainerResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, err := f.find(cid)
	if err != nil {
		return client.CopyFromContainerResult{}, err
	}
	src := strings.TrimSuffix(o.SourcePath, "/")
	base := src[strings.LastIndex(src, "/")+1:]
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// Like the real daemon: a directory source archives under its base name.
	_ = tw.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o755})
	found := false
	for p, b := range c.files {
		var name string
		switch {
		case p == src:
			name = base
		case strings.HasPrefix(p, src+"/"):
			name = base + "/" + strings.TrimPrefix(p, src+"/")
		default:
			continue
		}
		found = true
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))})
		_, _ = tw.Write(b)
	}
	_ = tw.Close()
	if !found {
		return client.CopyFromContainerResult{}, notFound("path " + o.SourcePath)
	}
	return client.CopyFromContainerResult{Content: io.NopCloser(&buf)}, nil
}

func (f *fakeEngine) ImageInspect(ctx context.Context, ref string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if err := f.err(); err != nil {
		return client.ImageInspectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.images[ref]
	if !ok {
		// An image ID inspects as itself.
		for _, v := range f.images {
			if v == ref {
				id, ok = ref, true
			}
		}
	}
	if !ok {
		return client.ImageInspectResult{}, notFound("image " + ref)
	}
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: id, RepoTags: []string{ref}}}, nil
}

func (f *fakeEngine) ImageBuild(ctx context.Context, buildContext io.Reader, o client.ImageBuildOptions) (client.ImageBuildResult, error) {
	if err := f.err(); err != nil {
		return client.ImageBuildResult{}, err
	}
	_, _ = io.Copy(io.Discard, buildContext)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tag := range o.Tags {
		f.images[tag] = "sha256:built-" + tag
	}
	body := io.NopCloser(strings.NewReader("{\"stream\":\"Step 1/1\"}\n"))
	return client.ImageBuildResult{Body: body}, nil
}

type pullResponse struct{ io.ReadCloser }

func (p pullResponse) JSONMessages(ctx context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {}
}
func (p pullResponse) Wait(ctx context.Context) error {
	_, err := io.Copy(io.Discard, p)
	return err
}

func (f *fakeEngine) ImagePull(ctx context.Context, ref string, o client.ImagePullOptions) (client.ImagePullResponse, error) {
	if err := f.err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.images[ref] = "sha256:pulled-" + ref
	f.mu.Unlock()
	return pullResponse{io.NopCloser(strings.NewReader("{\"status\":\"Pull complete\"}\n"))}, nil
}

// seed inserts a container directly (foreign/other-manager cases).
func (f *fakeEngine) seed(name string, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.nextID("cid")
	f.containers[id] = &fakeContainer{id: id, name: name, running: true,
		cfg: &container.Config{Image: "busybox:latest", Labels: labels}, files: map[string][]byte{}}
	f.byName[name] = id
	return id
}
