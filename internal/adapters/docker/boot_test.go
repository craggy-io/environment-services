package docker

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/internal/adapter"
)

// downAdapter is an adapter whose daemon address has nothing listening yet.
func downAdapter(t *testing.T) (*Adapter, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cli, err := client.New(client.WithHost("tcp://" + addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Adapter{cli: cli}, addr
}

// daemonAt answers pings on addr, as a started daemon would.
func daemonAt(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Error(err)
		return
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		w.WriteHeader(http.StatusOK)
	}))
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
}

func TestCheckNeverStartsDocker(t *testing.T) {
	d, _ := downAdapter(t)
	d.Start = func(context.Context) error {
		t.Error("Check started the runtime")
		return nil
	}
	if err := d.Check(context.Background()); !errors.Is(err, adapter.ErrBackendDown) {
		t.Fatalf("Check = %v, want ErrBackendDown", err)
	}
	// Backend down during recovery observes Unknown, never Absent.
	obs, err := d.Observe(context.Background(), adapter.OpContext{EnvID: testEnvID}, adapter.Ref{EnvID: testEnvID})
	if err != nil {
		t.Fatalf("Observe err = %v", err)
	}
	if obs.Presence != adapter.Unknown {
		t.Fatalf("Observe with daemon down = %s, want Unknown", obs.Presence)
	}
}

func TestStartBackendWaitsUntilTheDaemonAnswers(t *testing.T) {
	d, addr := downAdapter(t)
	starts := 0
	d.Start = func(context.Context) error {
		starts++
		time.AfterFunc(1500*time.Millisecond, func() { daemonAt(t, addr) })
		return nil
	}
	if err := d.StartBackend(context.Background(), nil); err != nil {
		t.Fatalf("StartBackend: %v", err)
	}
	if starts != 1 {
		t.Fatalf("started %d times, want 1", starts)
	}
	if err := d.Check(context.Background()); err != nil {
		t.Fatalf("Check after start: %v", err)
	}
}

func TestStartBackendGivesUpWhenNothingAnswers(t *testing.T) {
	d, _ := downAdapter(t)
	d.Start = func(context.Context) error { return nil }
	d.BootTimeout = 100 * time.Millisecond
	err := d.StartBackend(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("StartBackend = %v, want a did-not-answer error", err)
	}
}

func TestStartBackendReportsAStartFailure(t *testing.T) {
	d, _ := downAdapter(t)
	boom := errors.New("not installed")
	d.Start = func(context.Context) error { return boom }
	if err := d.StartBackend(context.Background(), nil); !errors.Is(err, boom) {
		t.Fatalf("StartBackend = %v, want %v", err, boom)
	}
}

func TestStartBackendRefusesARuntimeItCannotStart(t *testing.T) {
	d, _ := downAdapter(t)
	if err := d.StartBackend(context.Background(), nil); err == nil {
		t.Fatal("StartBackend with no starter must fail")
	}
}

func TestDockerHostDisablesStarting(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://remote:2375")
	if localStarter() != nil {
		t.Fatal("a DOCKER_HOST runtime must not be started locally")
	}
	t.Setenv("DOCKER_HOST", "")
	if localStarter() == nil {
		t.Fatal("the default runtime must be startable")
	}
}
