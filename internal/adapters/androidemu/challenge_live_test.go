package androidemu

// Live challenge test: a real emulator reaches a host-owned cleartext
// challenge through the emulator host alias, and destroying the
// environment closes it. Gated like TestLiveEmulatorLifecycle:
// CRAGGY_TEST_ANDROID_EMU=1.

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

func TestLiveEmulatorReachesChallenges(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_ANDROID_EMU") != "1" {
		t.Skip("set CRAGGY_TEST_ANDROID_EMU=1 to run against a real Android SDK")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	d := New(t.TempDir())
	if err := d.Check(ctx); err != nil {
		t.Skipf("android SDK unavailable: %v", err)
	}
	op := adapter.OpContext{EnvID: "env_live_challenge", Generation: 1}
	spec := envspec.Spec{Kind: envspec.Android, Profile: "android",
		Android: &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB}}
	inst, err := d.Create(ctx, op, spec, func(p envspec.Progress) { t.Logf("%s: %s", p.Stage, p.Message) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Dispose(context.Background(), op, adapter.Destroy) }()
	dev := inst.(*Instance).Device()

	clear, err := dev.StartCleartextChallenge(ctx, envspec.CleartextChallengeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tlsc, err := dev.StartTLSChallenge(ctx, envspec.TLSChallengeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(clear.URL())
	if err != nil || u.Hostname() != envspec.ChallengeHostAlias {
		t.Fatalf("cleartext url = %q err=%v", clear.URL(), err)
	}
	req := fmt.Sprintf(`printf 'GET %s HTTP/1.0\r\nHost: %s\r\n\r\n' | toybox nc -w 5 %s %s`, u.EscapedPath(), u.Host, u.Hostname(), u.Port())
	out, code, err := dev.Shell(ctx, req)
	if err != nil || code != 0 {
		t.Fatalf("device request = %q code=%d err=%v", out, code, err)
	}
	if r := clear.Receipt().Requests; len(r) != 1 || !r[0].Matched {
		t.Fatalf("cleartext receipt = %+v (device saw %q)", r, out)
	}

	// Destroy closes every challenge the device owns.
	if err := inst.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{clear.URL(), tlsc.URL()} {
		pu, _ := url.Parse(raw)
		if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", pu.Port()), time.Second); err == nil {
			conn.Close()
			t.Fatalf("%s still listening after destroy", raw)
		}
	}
}
