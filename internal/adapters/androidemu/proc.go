package androidemu

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// emulatorProcess returns the running emulator recorded as pid on
// console port, or nil when that PID is gone or now belongs to another
// program (Windows reuses PIDs, so a state file's PID alone proves
// nothing).
func emulatorProcess(pid, port int) *process.Process {
	if pid <= 0 {
		return nil
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return nil
	}
	name, err := p.Name()
	if err != nil {
		return nil
	}
	cmdline, _ := p.Cmdline()
	if !isEmulatorCommand(name, cmdline, port) {
		return nil
	}
	return p
}

// isEmulatorCommand reports whether a process image name and command
// line are an emulator launched on console port. An unreadable command
// line (empty) is judged by the name alone.
func isEmulatorCommand(name, cmdline string, port int) bool {
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
	if base != "emulator" && !strings.HasPrefix(base, "qemu-system-") {
		return false
	}
	if cmdline == "" {
		return true
	}
	return port > 0 && strings.Contains(" "+cmdline+" ", " -port "+strconv.Itoa(port)+" ")
}

// killTree kills p and everything it spawned. On Windows emulator.exe
// runs QEMU as a separate child process that a kill of the parent alone
// leaves running, still holding the AVD and its memory.
func killTree(p *process.Process) {
	children, _ := p.Children()
	_ = p.Kill()
	for _, c := range children {
		killTree(c)
	}
}
