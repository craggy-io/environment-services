//go:build !windows

package hostenv

import (
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// On Unix the "job" is a process group (cgroup limits are deployment
// concerns; lifecycle parity is what the driver needs).
type job struct {
	name string
	pgid int
}

func createJob(name string, memMB int64, maxProcs uint32) (*job, error) {
	return &job{name: name}, nil
}

func openJob(name string) (*job, error) { return &job{name: name}, nil }

func (j *job) assign(cmd *exec.Cmd) error {
	if j.pgid == 0 && cmd.Process != nil {
		j.pgid = cmd.Process.Pid
	}
	return nil
}

func (j *job) adopt(pid int) {
	if j.pgid == 0 {
		j.pgid = pid
	}
}

func (j *job) terminate() error {
	if j.pgid > 0 {
		return syscall.Kill(-j.pgid, syscall.SIGKILL)
	}
	return nil
}

func (j *job) close() {}

func (j *job) accounting() (time.Duration, uint32, error) { return 0, 0, nil }

func processCreationTime(pid int) (time.Time, bool) {
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return time.Time{}, false
	}
	createdMS, err := p.CreateTime()
	if err != nil || createdMS <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(createdMS), true
}

func killProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
