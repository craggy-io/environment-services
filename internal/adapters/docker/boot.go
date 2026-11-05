package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

const (
	// bootTimeout bounds a start: Docker Desktop's WSL2 VM takes a minute
	// or two from cold.
	bootTimeout = 3 * time.Minute
	bootPoll    = time.Second
	// startWaitDelay stops a start command being held open by a daemon
	// that inherited its output pipes.
	startWaitDelay = 5 * time.Second
)

// localStarter returns how to start the local runtime, or nil when a
// DOCKER_HOST names one this process cannot start (remote, Podman).
func localStarter() func(context.Context) error {
	if os.Getenv("DOCKER_HOST") != "" {
		return nil
	}
	return startDockerDesktop
}

// startDockerDesktop asks Docker Desktop to start without waiting for its
// engine; StartBackend does the waiting. Starting it twice is harmless.
func startDockerDesktop(ctx context.Context) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "desktop", "start", "--detach")
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = startWaitDelay
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("docker desktop start: %w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

// StartBackend starts the Docker runtime and waits until the daemon
// answers (adapter.BackendStarter). Check never starts anything, so
// only a caller that needs a container brings Docker up.
func (d *Adapter) StartBackend(ctx context.Context, progress func(envspec.Progress)) error {
	if d.Start == nil {
		return errors.New("the Docker runtime named by DOCKER_HOST cannot be started from here")
	}
	if progress != nil {
		progress(envspec.Progress{Stage: "backend", Message: "starting Docker"})
	}
	limit := d.BootTimeout
	if limit <= 0 {
		limit = bootTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		return err
	}
	tick := time.NewTicker(bootPoll)
	defer tick.Stop()
	for {
		err := d.Check(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("docker did not answer after starting: %w", err)
		case <-tick.C:
		}
	}
}
