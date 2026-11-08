package androiddev

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
)

func TestWaitDeviceReportsOfflineStateAtDeadline(t *testing.T) {
	tr := &fakeTransport{devices: []adb.Device{{Serial: "emulator-5556", State: "offline"}}}
	start := time.Now()
	err := WaitDevice(context.Background(), tr, "emulator-5556", 20*time.Millisecond)
	var wait *DeviceWaitError
	if !errors.As(err, &wait) || wait.Serial != "emulator-5556" || wait.State != "offline" {
		t.Fatalf("wait error = %v", err)
	}
	if !strings.Contains(err.Error(), "last adb state offline") || time.Since(start) > time.Second {
		t.Fatalf("wait did not honor deadline or retain state: %v", err)
	}
}

func TestWaitDeviceParentCancellationIsNotTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := WaitDevice(ctx, &fakeTransport{}, "emulator-5556", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait = %v", err)
	}
}
