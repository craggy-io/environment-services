package androidemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// bootFailure retains the bounded emulator output and the final adb view
// before cleanup removes the failed process. Both the operation error and
// the on-disk diagnostic identify the attempted serial and snapshot args.
func (d *Adapter) bootFailure(inst *Instance, args []string, output *syncBuffer, cause error) error {
	log := output.String()
	probeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	devices, adbErr := d.transport().Devices(probeCtx)
	state := "absent"
	for _, device := range devices {
		if device.Serial == inst.serial {
			state = device.State
			break
		}
	}
	if adbErr != nil {
		state = "adb error: " + adbErr.Error()
	}
	lines := []string{
		"time: " + time.Now().UTC().Format(time.RFC3339Nano),
		"avd: " + inst.avd,
		"serial: " + inst.serial,
		fmt.Sprintf("pid: %d", inst.pid),
		"args: " + strings.Join(args, " "),
		"last adb state: " + state,
		"failure: " + cause.Error(),
		"emulator stdout/stderr:",
		log,
	}
	path, saveErr := d.retainBootLog(strings.Join(lines, "\n"))
	tail := tailLines(log, 20)
	if tail == "" {
		tail = "<no emulator output>"
	}
	if saveErr != nil {
		return fmt.Errorf("%w (serial %s, pid %d, last adb state %s; could not retain emulator log: %v; emulator tail: %s)", cause, inst.serial, inst.pid, state, saveErr, tail)
	}
	return fmt.Errorf("%w (serial %s, pid %d, last adb state %s; emulator log %s; emulator tail: %s)", cause, inst.serial, inst.pid, state, path, tail)
}

func (d *Adapter) retainBootLog(text string) (string, error) {
	dir := filepath.Join(d.StateDir, "android", "boot-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "boot-*.log")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err = file.WriteString(text); err != nil {
		_ = file.Close()
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func (d *Adapter) cleanupFailedBoot(inst *Instance) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = inst.destroy(ctx)
}
