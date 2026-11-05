package adb_test

import (
	"context"
	"os"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
)

// Live forward check: our client's Forward must carry a real Frida
// session (frida-ps against an on-device frida-server). Gated:
// CRAGGY_TEST_ADB_FORWARD=1 with a booted emulator-5554 running
// frida-server on 27042.
func TestForwardLiveFrida(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_ADB_FORWARD") != "1" {
		t.Skip("set CRAGGY_TEST_ADB_FORWARD=1 with a frida-server running on emulator-5554:27042")
	}
	serial := os.Getenv("CRAGGY_ADB_SERIAL")
	if serial == "" {
		serial = "emulator-5554"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := adb.New()
	if _, err := c.Forward(ctx, serial, 27050, 27042); err != nil {
		t.Fatalf("forward: %v", err)
	}
	defer c.Forward(context.Background(), serial, 27050, 0)
	out, err := osexec.CommandContext(ctx, "frida-ps", "-H", "127.0.0.1:27050").CombinedOutput()
	if err != nil {
		t.Fatalf("frida-ps: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PID") {
		t.Fatalf("frida-ps output unexpected:\n%s", out)
	}
	t.Logf("frida-ps over our forward OK:\n%.300s", out)
}
