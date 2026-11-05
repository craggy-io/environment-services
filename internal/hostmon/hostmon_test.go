package hostmon_test

import (
	"context"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/hostmon"
)

func TestCapacityReserveAndPressure(t *testing.T) {
	usedPct := 50.0
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pressured := make(chan envspec.HostSnapshot, 4)
	mon := hostmon.New(hostmon.Config{
		ReservePct: 0.25, ReserveCPUs: 2, PressurePct: 90, PressureFor: 10 * time.Second,
		OnPressure: func(s envspec.HostSnapshot) { pressured <- s },
		Sampler: func() (envspec.HostSnapshot, error) {
			now = now.Add(time.Second)
			return envspec.HostSnapshot{Time: now, CPUs: 16, CPUPercent: 10,
				MemTotalMB: 32000, MemFreeMB: 16000, MemUsedPct: usedPct}, nil
		},
	})
	if err := mon.Sample(); err != nil {
		t.Fatal(err)
	}
	cap := mon.HostCapacity()
	// Budget = 75% of 32000 = 24000; free = 16000 - 8000 reserve = 8000.
	if cap.MemoryMB != 24000 || cap.FreeMB != 8000 || cap.CPUs != 14 {
		t.Fatalf("capacity = %+v", cap)
	}
	mon.SetDockerCapacity(8192, 8)
	if dc := mon.DockerCapacity(); dc.MemoryMB != 8192 || dc.CPUs != 8 {
		t.Fatalf("docker capacity = %+v", dc)
	}
	// Pressure needs >90% sustained for 10s of samples.
	usedPct = 95
	for i := 0; i < 9; i++ {
		_ = mon.Sample()
	}
	if mon.Pressure() {
		t.Fatal("pressure must not fire before the sustained window")
	}
	for i := 0; i < 3; i++ {
		_ = mon.Sample()
	}
	if !mon.Pressure() {
		t.Fatal("pressure should fire after 10s above 90%")
	}
	select {
	case <-pressured:
	default:
		t.Fatal("OnPressure not called")
	}
	usedPct = 10
	_ = mon.Sample()
	if mon.Pressure() {
		t.Fatal("pressure should clear when usage drops")
	}
	_ = context.Background()
}
