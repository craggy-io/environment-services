package adb_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
)

// Live check that uiautomator works over our shell-v2 transport (the
// Magisk grant flow depends on it). Gated: CRAGGY_TEST_ADB_FORWARD=1
// with any device online.
func TestUIAutomatorLive(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_ADB_FORWARD") != "1" {
		t.Skip("set CRAGGY_TEST_ADB_FORWARD=1 with a device online")
	}
	serial := os.Getenv("CRAGGY_ADB_SERIAL")
	if serial == "" {
		serial = "emulator-5554"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := adb.New()
	out, code, err := c.Run(ctx, serial, "uiautomator dump /sdcard/craggy-ui.xml")
	t.Logf("dump: code=%d err=%v out=%q", code, err, out)
	out2, code, err := c.Run(ctx, serial, "wc -c /sdcard/craggy-ui.xml; cat /sdcard/craggy-ui.xml")
	t.Logf("cat: code=%d err=%v len=%d head=%.120q", code, err, len(out2), out2)
	if !strings.Contains(out2, "<hierarchy") {
		t.Fatalf("no hierarchy in dump (len %d)", len(out2))
	}
	out3, code, err := c.Run(ctx, serial, "input tap 400 590; echo TAPPED")
	t.Logf("tap: code=%d err=%v out=%q", code, err, out3)
	out4, code, err := c.Run(ctx, serial, "monkey -p com.topjohnwu.magisk -c android.intent.category.LAUNCHER 1")
	t.Logf("monkey: code=%d err=%v out=%.200q", code, err, out4)
}
