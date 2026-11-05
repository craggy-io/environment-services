package core

import (
	"bufio"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

//go:embed assets/linux-tools/Dockerfile
var assets embed.FS

// linuxToolsBuild is the linux-tools profile's Docker build context.
var linuxToolsBuild fs.FS

func init() {
	sub, err := fs.Sub(assets, "assets/linux-tools")
	if err != nil {
		panic(fmt.Sprintf("core: linux-tools build context: %v", err))
	}
	linuxToolsBuild = sub
}

// StockProfiles are the built-in set, named by what they are for.
func StockProfiles() []envspec.Profile {
	allow := []string{"debian:*", "eclipse-temurin:*", "craggy/*", "alpine:*", "busybox:*"}
	return []envspec.Profile{
		{
			Name: "linux-tools", Kind: envspec.Container,
			Description:  "Debian with jadx, apktool, radare2, binwalk and Python; general static analysis",
			Build:        &envspec.Build{Context: linuxToolsBuild},
			Resources:    envspec.Resources{CPUs: 2, MemoryMB: 2048},
			MaxResources: envspec.Resources{CPUs: 8, MemoryMB: 16384, DiskMB: 102400},
			AllowImages:  allow, Network: envspec.NetworkNone,
			AllowNetworks: []envspec.Network{envspec.NetworkNone, envspec.NetworkIsolated, envspec.NetworkEgress},
			TTL:           time.Hour, MaxTTL: 8 * time.Hour, Prepare: true,
		},
		{
			Name: "android", Kind: envspec.Android,
			Description:   "The default for dynamic analysis: google_apis x86_64, rooted, Frida ready, proxy CA installed",
			Resources:     envspec.Resources{CPUs: 2, MemoryMB: 3072, DiskMB: 8192},
			MaxResources:  envspec.Resources{CPUs: 4, MemoryMB: 8192, DiskMB: 32768},
			Network:       envspec.NetworkEgress,
			AllowNetworks: []envspec.Network{envspec.NetworkNone, envspec.NetworkEgress, envspec.NetworkOpen},
			TTL:           2 * time.Hour, MaxTTL: 12 * time.Hour, Reuse: true,
			Android: &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB,
				Frida: &envspec.FridaOptions{}, ProxyCA: true},
		},
		{
			Name: "android-play", Kind: envspec.Android,
			Description:   "Stock google_apis_playstore, unrooted; when an app must come from the Play Store or you want unmodified-device behaviour",
			Resources:     envspec.Resources{CPUs: 2, MemoryMB: 3072, DiskMB: 8192},
			MaxResources:  envspec.Resources{CPUs: 4, MemoryMB: 8192, DiskMB: 32768},
			Network:       envspec.NetworkEgress,
			AllowNetworks: []envspec.Network{envspec.NetworkNone, envspec.NetworkEgress, envspec.NetworkOpen},
			TTL:           2 * time.Hour, MaxTTL: 12 * time.Hour, Reuse: true,
			Android: &envspec.AndroidOptions{API: 34, Image: envspec.GooglePlay, Root: envspec.RootNone},
		},
		{
			Name: "android-play-rooted", Kind: envspec.Android,
			Description:   "google_apis_playstore rooted with Magisk, Frida ready; Play Store apps that also need instrumentation",
			Resources:     envspec.Resources{CPUs: 2, MemoryMB: 3072, DiskMB: 8192},
			MaxResources:  envspec.Resources{CPUs: 4, MemoryMB: 8192, DiskMB: 32768},
			Network:       envspec.NetworkEgress,
			AllowNetworks: []envspec.Network{envspec.NetworkNone, envspec.NetworkEgress, envspec.NetworkOpen},
			TTL:           2 * time.Hour, MaxTTL: 12 * time.Hour, Reuse: true,
			Android: &envspec.AndroidOptions{API: 34, Image: envspec.GooglePlay, Root: envspec.RootMagisk,
				Frida: &envspec.FridaOptions{}, ProxyCA: true},
		},
		{
			Name: "host-tools", Kind: envspec.Host,
			Description:   "The machine's own toolchain (Android SDK build-tools, platform-tools, anything installed). For tools that only run on the host. Not for samples.",
			Resources:     envspec.Resources{CPUs: 2, MemoryMB: 4096},
			MaxResources:  envspec.Resources{CPUs: 8, MemoryMB: 32768},
			Network:       envspec.NetworkOpen,
			AllowNetworks: []envspec.Network{envspec.NetworkOpen},
			TTL:           time.Hour, MaxTTL: 8 * time.Hour,
			Host: &envspec.HostOptions{},
		},
	}
}

// DefaultProfileFor returns the default profile name for a kind.
func DefaultProfileFor(k envspec.Kind) string {
	switch k {
	case envspec.Android:
		return "android"
	case envspec.Host:
		return "host-tools"
	default:
		return "linux-tools"
	}
}

type profileState struct {
	p         envspec.Profile
	digest    string
	imageRef  string
	imageID   string
	preparing bool
	prepErr   string
	ready     bool
}

// BuildDigest is the content digest (12 hex) of a profile's Dockerfile,
// build context and args. Identical contexts share one image.
func BuildDigest(p envspec.Profile) (string, error) {
	if p.Build == nil || p.Build.Context == nil {
		return "", nil
	}
	h := sha256.New()
	var names []string
	err := fs.WalkDir(p.Build.Context, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	for _, n := range names {
		f, err := p.Build.Context.Open(n)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "file:%s\n", n)
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	argNames := make([]string, 0, len(p.Build.Args))
	for k := range p.Build.Args {
		argNames = append(argNames, k)
	}
	sort.Strings(argNames)
	for _, k := range argNames {
		fmt.Fprintf(h, "arg:%s=%s\n", k, p.Build.Args[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:12], nil
}

// ImageTagFor returns the content-tagged image ref for a Build profile.
func ImageTagFor(name, digest string) string {
	clean := strings.ReplaceAll(strings.ToLower(name), "_", "-")
	return "craggy/" + clean + ":" + digest
}

func dockerfileFroms(r io.Reader) ([]string, error) {
	var out []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
			i := 1
			for i < len(fields) && strings.HasPrefix(fields[i], "--") {
				i++
			}
			if i < len(fields) {
				out = append(out, fields[i])
			}
		}
		low := strings.ToLower(line)
		if strings.Contains(low, "--privileged") || strings.Contains(low, "security-opt") {
			return out, Invalidf("Dockerfile requests privileged-style option: %q", line)
		}
	}
	return out, sc.Err()
}

func imageAllowed(image string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	cands := []string{image}
	if !strings.Contains(path.Base(image), ":") && !strings.Contains(image, "@") {
		cands = append(cands, image+":latest")
	}
	for _, pat := range allow {
		for _, c := range cands {
			if ok, _ := path.Match(pat, c); ok {
				return true
			}
		}
	}
	return false
}

// validateProfile enforces registration rules: name shape, Image xor
// Build, allowlist (incl. Dockerfile FROM lines), ceilings.
func validateProfile(p envspec.Profile) error {
	if p.Name == "" {
		return Invalidf("profile has no name")
	}
	if strings.ContainsAny(p.Name, " \t/\\") {
		return Invalidf("profile name %q must be a dotted identifier (its image namespace)", p.Name)
	}
	if p.Image != "" && p.Build != nil {
		return Invalidf("profile %s sets both Image and Build; choose one", p.Name)
	}
	if p.Image != "" && !imageAllowed(p.Image, p.AllowImages) {
		return WithHint("unavailable", Invalidf("image %q is outside profile %s's base-image allowlist", p.Image, p.Name),
			"choose an image matching one of %v", p.AllowImages)
	}
	if p.Build != nil {
		if p.Build.Context == nil {
			return Invalidf("profile %s Build has no Context", p.Name)
		}
		df := p.Build.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		f, err := p.Build.Context.Open(df)
		if err != nil {
			return Invalidf("profile %s: open %s: %v", p.Name, df, err)
		}
		froms, err := dockerfileFroms(f)
		f.Close()
		if err != nil {
			return err
		}
		for _, from := range froms {
			if from == "scratch" {
				continue
			}
			if !imageAllowed(from, p.AllowImages) {
				return WithHint("unavailable", Invalidf("profile %s Dockerfile FROM %q is outside the base-image allowlist", p.Name, from),
					"use a base matching one of %v", p.AllowImages)
			}
		}
	}
	if p.MaxResources.MemoryMB > 0 && p.Resources.MemoryMB > p.MaxResources.MemoryMB ||
		p.MaxResources.CPUs > 0 && p.Resources.CPUs > p.MaxResources.CPUs {
		return Invalidf("profile %s defaults exceed its own ceilings", p.Name)
	}
	return nil
}

// mergeSpec applies profile defaults under spec overrides and enforces
// ceilings: an agent can't exceed them.
func mergeSpec(p envspec.Profile, s envspec.Spec) (envspec.Spec, error) {
	out := s
	out.Kind = p.Kind
	if out.Profile == "" {
		out.Profile = p.Name
	}
	if out.Image == "" {
		out.Image = p.Image
	}
	if out.Image != "" && !imageAllowed(out.Image, p.AllowImages) {
		return out, WithHint("unavailable", Invalidf("image %q is outside profile %s's allowlist", out.Image, p.Name),
			"provision with profile %q and no image override, or an allowed image %v", p.Name, p.AllowImages)
	}
	if out.Resources.CPUs == 0 {
		out.Resources.CPUs = p.Resources.CPUs
	}
	if out.Resources.MemoryMB == 0 {
		out.Resources.MemoryMB = p.Resources.MemoryMB
	}
	if out.Resources.DiskMB == 0 {
		out.Resources.DiskMB = p.Resources.DiskMB
	}
	if p.MaxResources.CPUs > 0 && out.Resources.CPUs > p.MaxResources.CPUs {
		return out, WithHint("invalid", Invalidf("cpus %g exceed profile %s ceiling %g", out.Resources.CPUs, p.Name, p.MaxResources.CPUs),
			"lower resources.cpus to at most %g", p.MaxResources.CPUs)
	}
	if p.MaxResources.MemoryMB > 0 && out.Resources.MemoryMB > p.MaxResources.MemoryMB {
		return out, WithHint("invalid", Invalidf("memory_mb %d exceeds profile %s ceiling %d", out.Resources.MemoryMB, p.Name, p.MaxResources.MemoryMB),
			"lower resources.memory_mb to at most %d", p.MaxResources.MemoryMB)
	}
	if p.MaxResources.DiskMB > 0 && out.Resources.DiskMB > p.MaxResources.DiskMB {
		return out, Invalidf("disk_mb %d exceeds profile %s ceiling %d", out.Resources.DiskMB, p.Name, p.MaxResources.DiskMB)
	}
	if out.Network == "" {
		out.Network = p.Network
	}
	if len(p.AllowNetworks) > 0 {
		ok := false
		for _, n := range p.AllowNetworks {
			if n == out.Network {
				ok = true
			}
		}
		if !ok {
			return out, WithHint("invalid", Invalidf("network %q not allowed by profile %s", out.Network, p.Name),
				"use one of %v", p.AllowNetworks)
		}
	}
	if out.TTL == 0 {
		out.TTL = p.TTL
	}
	if p.MaxTTL > 0 && out.TTL > p.MaxTTL {
		return out, Invalidf("ttl %s exceeds profile %s ceiling %s", out.TTL, p.Name, p.MaxTTL)
	}
	if out.Android == nil && p.Android != nil {
		cp := *p.Android
		out.Android = &cp
	}
	if out.Host == nil && p.Host != nil {
		cp := *p.Host
		out.Host = &cp
	}
	if out.Labels == nil && p.Labels != nil {
		out.Labels = map[string]string{}
		for k, v := range p.Labels {
			out.Labels[k] = v
		}
	}
	if out.Params == nil && p.Params != nil {
		out.Params = map[string]string{}
		for k, v := range p.Params {
			out.Params[k] = v
		}
	}
	return out, nil
}

// RegisterProfiles validates and registers profiles. Re-registering
// the same name replaces it; running environments keep their image.
func (m *Manager) RegisterProfiles(ps ...envspec.Profile) error {
	for _, p := range ps {
		if err := validateProfile(p); err != nil {
			return err
		}
		digest, err := BuildDigest(p)
		if err != nil {
			return err
		}
		m.mu.Lock()
		old := m.profiles[p.Name]
		st := &profileState{p: p, digest: digest}
		if old != nil && old.digest == digest && old.p.Image == p.Image {
			st = old
			st.p = p
		}
		m.profiles[p.Name] = st
		m.mu.Unlock()
	}
	return nil
}

// Profiles lists registered profiles.
func (m *Manager) Profiles() []envspec.Profile {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]envspec.Profile, 0, len(m.profiles))
	for _, ps := range m.profiles {
		out = append(out, ps.p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Profile returns one registered profile.
func (m *Manager) Profile(name string) (envspec.Profile, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps, ok := m.profiles[name]
	if !ok {
		return envspec.Profile{}, false
	}
	return ps.p, true
}

// ProfileStatus is the authoritative readiness answer for one profile.
func (m *Manager) ProfileStatus(name string) envspec.ProfileStatus {
	m.mu.Lock()
	ps, ok := m.profiles[name]
	m.mu.Unlock()
	if !ok {
		return envspec.ProfileStatus{Ready: false, NotReady: "unknown profile"}
	}
	st := envspec.ProfileStatus{Profile: ps.p}
	switch {
	case ps.prepErr != "":
		st.NotReady, st.State = ps.prepErr, "failed"
	case ps.preparing:
		st.NotReady, st.State = "preparing", "preparing"
	default:
		if err := m.profileReady(name); err != nil {
			st.NotReady, st.State = err.Error(), "not_ready"
		} else {
			st.Ready, st.State = true, "ready"
		}
	}
	return st
}

func (m *Manager) profileReady(name string) error {
	m.mu.Lock()
	ps, ok := m.profiles[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: profile %q", envspec.ErrNotFound, name)
	}
	prep, perr, ready := ps.preparing, ps.prepErr, ps.ready
	kind, prof := ps.p.Kind, ps.p
	m.mu.Unlock()
	if perr != "" {
		return fmt.Errorf("profile %s preparation failed: %s", name, perr)
	}
	if prep {
		return fmt.Errorf("%w: profile %s is still preparing", envspec.ErrNotReady, name)
	}
	if ready {
		return nil
	}
	ad, found := m.adapterFor(kind, false)
	if !found {
		return fmt.Errorf("%w: no adapter for kind %q", envspec.ErrNotReady, kind)
	}
	if b, ok := ad.(adapter.ImageBuilder); ok && kind == envspec.Container {
		ref := prof.Image
		m.mu.Lock()
		if ps2 := m.profiles[name]; ps2 != nil && ps2.digest != "" {
			ref = ImageTagFor(name, ps2.digest)
		}
		m.mu.Unlock()
		id, exists, err := b.ImageExists(context.Background(), ref)
		if err != nil {
			return fmt.Errorf("%w: profile %s: the container runtime is not answering; it starts on first need", envspec.ErrNotReady, name)
		}
		if exists {
			m.mu.Lock()
			if ps2 := m.profiles[name]; ps2 != nil {
				ps2.ready, ps2.imageRef, ps2.imageID = true, ref, id
			}
			m.mu.Unlock()
			return nil
		}
		return fmt.Errorf("%w: profile %s image %s not built/pulled yet", envspec.ErrNotReady, name, ref)
	}
	if prep2, ok := ad.(adapter.Preparer); ok {
		return prep2.ProfilePrepared(prof)
	}
	return nil
}

// PrepareProfile runs a profile's preparation now (image build/pull or
// adapter preparation), streaming progress.
func (m *Manager) PrepareProfile(ctx context.Context, name string, progress func(envspec.Progress)) error {
	if err := m.EnsureProfileBackend(ctx, name, progress); err != nil {
		return err
	}
	return m.ensureProfileImage(ctx, name, progress)
}

// EnsureProfileBackend starts a profile's runtime if stopped.
func (m *Manager) EnsureProfileBackend(ctx context.Context, name string, progress func(envspec.Progress)) error {
	p, ok := m.Profile(name)
	if !ok {
		return fmt.Errorf("%w: profile %q", envspec.ErrNotFound, name)
	}
	ad, found := m.adapterFor(p.Kind, false)
	if !found {
		return nil
	}
	return m.ensureBackend(ctx, p.Kind, ad, progress)
}

func (m *Manager) ensureBackend(ctx context.Context, kind envspec.Kind, ad adapter.Adapter, progress func(envspec.Progress)) error {
	chk, isChk := ad.(adapter.Checker)
	if !isChk {
		return nil
	}
	if err := chk.Check(ctx); err == nil {
		m.refreshDockerCapacity(ctx, kind, ad)
		return nil
	}
	starter, ok := ad.(adapter.BackendStarter)
	if !ok {
		return chk.Check(ctx)
	}
	select {
	case m.backendStart <- struct{}{}:
		defer func() { <-m.backendStart }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := chk.Check(ctx); err == nil {
		return nil
	}
	if err := starter.StartBackend(ctx, progress); err != nil {
		return err
	}
	m.emit("backend.started", "", map[string]any{"kind": string(kind)})
	m.refreshDockerCapacity(ctx, kind, ad)
	m.prepareProfiles(kind)
	return nil
}

// refreshDockerCapacity re-reads the container backend's totals into
// the host monitor; a no-op for other kinds and when the backend
// cannot report (capacity is seeded again on the next ensure).
func (m *Manager) refreshDockerCapacity(ctx context.Context, kind envspec.Kind, ad adapter.Adapter) {
	if m.cfg.Host == nil || kind != envspec.Container {
		return
	}
	if r, ok := ad.(adapter.CapacityReporter); ok {
		if memMB, cpus, err := r.Capacity(ctx); err == nil {
			m.cfg.Host.SetDockerCapacity(memMB, cpus)
		}
	}
}

func (m *Manager) prepareProfiles(kind envspec.Kind) {
	m.mu.Lock()
	var names []string
	for name, ps := range m.profiles {
		if ps.p.Prepare && ps.p.Kind == kind {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	for _, name := range names {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			_ = m.ensureProfileImage(m.ctx, name, func(envspec.Progress) {})
		}()
	}
}

func (m *Manager) ensureProfileImage(ctx context.Context, name string, progress func(envspec.Progress)) error {
	m.mu.Lock()
	ps, ok := m.profiles[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: profile %q", envspec.ErrNotFound, name)
	}
	if ps.ready && ps.p.Kind == envspec.Container {
		m.mu.Unlock()
		return nil
	}
	p, digest := ps.p, ps.digest
	m.mu.Unlock()
	if progress == nil {
		progress = func(envspec.Progress) {}
	}
	ad, found := m.adapterFor(p.Kind, false)
	if p.Kind != envspec.Container {
		if !found {
			return nil
		}
		prep, ok := ad.(adapter.Preparer)
		if !ok {
			return nil
		}
		m.setPrep(name, true, "")
		err := MarkAs(ErrPreparation, prep.PrepareProfile(ctx, p, progress))
		m.setPrep(name, false, errText(err))
		if err == nil {
			m.mu.Lock()
			if ps2, ok := m.profiles[name]; ok {
				ps2.ready = true
			}
			m.mu.Unlock()
		}
		return err
	}
	if !found {
		return nil
	}
	builder, ok := ad.(adapter.ImageBuilder)
	if !ok {
		return nil // adapter pulls on create
	}
	m.setPrep(name, true, "")
	ref, imageID, err := builder.EnsureImage(ctx, p, digest, progress)
	err = MarkAs(ErrImageUnavailable, err)
	m.mu.Lock()
	if ps2, ok := m.profiles[name]; ok {
		ps2.preparing = false
		if err != nil {
			ps2.prepErr = err.Error()
		} else {
			ps2.prepErr = ""
			ps2.imageRef, ps2.imageID, ps2.ready = ref, imageID, true
		}
	}
	m.mu.Unlock()
	return err
}

func (m *Manager) setPrep(name string, preparing bool, errStr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps, ok := m.profiles[name]; ok {
		ps.preparing = preparing
		if errStr != "" {
			ps.prepErr = errStr
		}
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (m *Manager) profileReuses(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps := m.profiles[name]; ps != nil {
		return ps.p.Reuse
	}
	return false
}

func (m *Manager) profileDigest(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ps := m.profiles[name]; ps != nil {
		return ps.digest
	}
	return ""
}
