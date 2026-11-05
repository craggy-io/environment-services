package androidemu

import (
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

func TestWindowArgsDefaultsHeadless(t *testing.T) {
	if got := windowArgs(false); !reflect.DeepEqual(got, []string{"-no-window"}) {
		t.Fatalf("background = %v", got)
	}
	if got := windowArgs(true); len(got) != 0 {
		t.Fatalf("foreground = %v, want no flags", got)
	}
}

func TestParamTrue(t *testing.T) {
	s := envspec.Spec{Params: map[string]string{"avd.foreground": "true", "avd.writable": "false"}}
	if !paramTrue(s, "avd.foreground") || paramTrue(s, "avd.writable") || paramTrue(s, "missing") {
		t.Fatal("paramTrue misread params")
	}
}

func TestHeartbeatReportsUntilStopped(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 5 * time.Millisecond
	defer func() { heartbeatEvery = old }()
	var mu sync.Mutex
	var got []envspec.Progress
	stop := heartbeat(func(p envspec.Progress) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	}, envspec.Progress{Stage: "wait-boot-completed", Message: "waiting"})
	time.Sleep(40 * time.Millisecond)
	stop()
	mu.Lock()
	n := len(got)
	mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if n < 3 || len(got) != n {
		t.Fatalf("beats = %d before stop, %d after; want >=3 and none after stop", n, len(got))
	}
	for _, p := range got {
		if p.Stage != "wait-boot-completed" {
			t.Fatalf("beat changed stage: %+v", p)
		}
	}
	if !strings.Contains(got[len(got)-1].Message, "elapsed") {
		t.Fatalf("beat message %q does not say how long it has waited", got[len(got)-1].Message)
	}
	heartbeat(nil, envspec.Progress{})() // nil sink: no goroutine, stop is a no-op
}
