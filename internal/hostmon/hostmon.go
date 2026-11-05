// Package hostmon samples the host (CPU, memory, disk) for admission and
// the host.metrics@1 stream. It is a pure sampler: it returns
// envspec.HostSnapshot / envspec.HostCapacity records and knows nothing
// about signals or environments. Admission uses two pools — host and the
// Docker VM — because they run out separately on Windows.
package hostmon

import (
	"context"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/craggy-io/environment-services/envspec"
)

// Config tunes a Monitor.
type Config struct {
	Interval    time.Duration // default 1s
	Paths       []string      // disk-free watch paths
	ReservePct  float64       // host RAM reserve fraction; default 0.25
	ReserveCPUs float64       // default 2
	// PressurePct and PressureFor define pressure (default 90% for 10s).
	PressurePct float64
	PressureFor time.Duration
	// OnPressure is called when pressure starts (host.pressure).
	OnPressure func(envspec.HostSnapshot)
	// Sampler overrides sampling (tests).
	Sampler func() (envspec.HostSnapshot, error)
}

// Monitor samples on an interval and keeps the latest snapshot.
type Monitor struct {
	cfg         Config
	mu          sync.RWMutex
	latest      envspec.HostSnapshot
	have        bool
	subs        []func(envspec.HostSnapshot)
	hotSince    time.Time
	pressure    bool
	dockerMemMB int64
	dockerCPUs  int
}

// New builds a Monitor. Start begins sampling.
func New(cfg Config) *Monitor {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.ReservePct == 0 {
		cfg.ReservePct = 0.25
	}
	if cfg.ReserveCPUs == 0 {
		cfg.ReserveCPUs = 2
	}
	if cfg.PressurePct == 0 {
		cfg.PressurePct = 90
	}
	if cfg.PressureFor == 0 {
		cfg.PressureFor = 10 * time.Second
	}
	return &Monitor{cfg: cfg}
}

// Start samples until ctx ends. The first sample is taken synchronously.
func (m *Monitor) Start(ctx context.Context) {
	_ = m.Sample()
	go func() {
		t := time.NewTicker(m.cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = m.Sample()
			}
		}
	}()
}

// Sample takes one sample now and notifies subscribers.
func (m *Monitor) Sample() error {
	var snap envspec.HostSnapshot
	var err error
	if m.cfg.Sampler != nil {
		snap, err = m.cfg.Sampler()
		if err != nil {
			return err
		}
	} else {
		snap = sampleSystem(m.cfg.Paths)
	}
	m.mu.Lock()
	snap.DockerMemTotalMB = m.dockerMemMB
	snap.DockerCPUs = m.dockerCPUs
	if snap.MemUsedPct > m.cfg.PressurePct {
		if m.hotSince.IsZero() {
			m.hotSince = snap.Time
		}
		if snap.Time.Sub(m.hotSince) >= m.cfg.PressureFor && !m.pressure {
			m.pressure = true
		}
	} else {
		m.hotSince = time.Time{}
		m.pressure = false
	}
	snap.Pressure = m.pressure
	wasPressure := m.latest.Pressure
	m.latest = snap
	m.have = true
	subs := append([]func(envspec.HostSnapshot){}, m.subs...)
	m.mu.Unlock()
	for _, fn := range subs {
		fn(snap)
	}
	if snap.Pressure && !wasPressure && m.cfg.OnPressure != nil {
		m.cfg.OnPressure(snap)
	}
	return nil
}

// Latest returns the most recent snapshot.
func (m *Monitor) Latest() (envspec.HostSnapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latest, m.have
}

// Subscribe registers a per-sample callback (the hub publishes these as
// host.metrics@1; Monitor itself stays signal-free).
func (m *Monitor) Subscribe(fn func(envspec.HostSnapshot)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, fn)
}

// SetDockerCapacity records the Docker VM's totals (from `docker info`).
func (m *Monitor) SetDockerCapacity(memTotalMB int64, cpus int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dockerMemMB, m.dockerCPUs = memTotalMB, cpus
}

// HostCapacity returns the host pool budget (RAM/cores minus reserve,
// capped by measured free).
func (m *Monitor) HostCapacity() envspec.HostCapacity {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.latest
	if !m.have {
		return envspec.HostCapacity{}
	}
	budget := int64(float64(s.MemTotalMB) * (1 - m.cfg.ReservePct))
	free := s.MemFreeMB - int64(float64(s.MemTotalMB)*m.cfg.ReservePct)
	if free < 0 {
		free = 0
	}
	cpus := float64(s.CPUs) - m.cfg.ReserveCPUs
	if cpus < 0 {
		cpus = 0
	}
	freeCPUs := cpus * (1 - s.CPUPercent/100)
	if freeCPUs < 0 {
		freeCPUs = 0
	}
	return envspec.HostCapacity{CPUs: cpus, MemoryMB: budget, FreeMB: free, FreeCPUs: freeCPUs}
}

// DockerCapacity returns the Docker VM pool budget.
func (m *Monitor) DockerCapacity() envspec.HostCapacity {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return envspec.HostCapacity{CPUs: float64(m.dockerCPUs), MemoryMB: m.dockerMemMB,
		FreeMB: m.dockerMemMB, FreeCPUs: float64(m.dockerCPUs)}
}

// PoolCapacity returns one admission pool's budget. Reserved fields are
// zero: what live reservations hold is core's ledger, not the sampler's.
func (m *Monitor) PoolCapacity(pool envspec.Pool) envspec.PoolCapacity {
	c := m.HostCapacity()
	if pool == envspec.PoolDocker {
		c = m.DockerCapacity()
	}
	return envspec.PoolCapacity{Pool: pool, CPUs: c.CPUs, MemoryMB: c.MemoryMB,
		FreeMB: c.FreeMB, FreeCPUs: c.FreeCPUs}
}

// Pressure reports current memory pressure.
func (m *Monitor) Pressure() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pressure
}

func sampleSystem(paths []string) envspec.HostSnapshot {
	snap := envspec.HostSnapshot{Time: time.Now(), DiskFreeMB: map[string]int64{}}
	if n, err := cpu.Counts(true); err == nil {
		snap.CPUs = n
	}
	if pcts, err := cpu.Percent(0, false); err == nil && len(pcts) > 0 {
		snap.CPUPercent = pcts[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		snap.MemTotalMB = int64(vm.Total >> 20)
		snap.MemFreeMB = int64(vm.Available >> 20)
		snap.MemUsedPct = vm.UsedPercent
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if u, err := disk.Usage(p); err == nil {
			snap.DiskFreeMB[p] = int64(u.Free >> 20)
		}
	}
	if procs, err := process.Processes(); err == nil {
		for _, p := range procs {
			name, err := p.Name()
			if err != nil {
				continue
			}
			if name == "qemu-system-x86_64.exe" || name == "qemu-system-x86_64" ||
				name == "emulator.exe" || name == "crashpad_handler.exe" {
				if !isQemu(name) {
					continue
				}
				load := envspec.ProcLoad{PID: p.Pid, Name: name}
				if pct, err := p.CPUPercent(); err == nil {
					load.CPUPercent = pct
				}
				if mi, err := p.MemoryInfo(); err == nil && mi != nil {
					load.RSSMB = int64(mi.RSS >> 20)
				}
				snap.Emulators = append(snap.Emulators, load)
			}
		}
	}
	return snap
}

func isQemu(name string) bool {
	return len(name) >= 4 && name[:4] == "qemu"
}
