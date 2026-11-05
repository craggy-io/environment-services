//go:build windows

package hostenv

import (
	"fmt"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// job is a named Windows Job Object, not kill-on-close, so processes
// outlive the manager (rule 9) and Adopt can reopen it by name (§3.8).
type job struct {
	h    windows.Handle
	name string
}

func (j *job) adopt(int) {}

func createJob(name string, memMB int64, maxProcs uint32) (*job, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateJobObject(nil, name16)
	if err != nil {
		return nil, fmt.Errorf("hostenv: CreateJobObject %s: %w", name, err)
	}
	j := &job{h: h, name: name}
	if err := j.setLimits(memMB, maxProcs); err != nil {
		// Limits are best-effort; lifecycle is the job's purpose.
		_ = err
	}
	return j, nil
}

var procOpenJobObjectW = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")

const jobObjectAllAccess = 0x1F001F

func openJob(name string) (*job, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	r, _, callErr := procOpenJobObjectW.Call(jobObjectAllAccess, 0, uintptr(unsafe.Pointer(name16)))
	if r == 0 {
		return nil, fmt.Errorf("hostenv: OpenJobObject %s: %w", name, callErr)
	}
	return &job{h: windows.Handle(r), name: name}, nil
}

func (j *job) setLimits(memMB int64, maxProcs uint32) error {
	var basic windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	basic.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	// Deliberately NOT kill-on-close: remove that flag.
	basic.BasicLimitInformation.LimitFlags = 0
	if maxProcs > 0 {
		basic.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		basic.BasicLimitInformation.ActiveProcessLimit = maxProcs
	}
	if memMB > 0 {
		basic.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		basic.JobMemoryLimit = uintptr(memMB) << 20
	}
	_, err := windows.SetInformationJobObject(j.h, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)))
	return err
}

func (j *job) assign(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("hostenv: process not started")
	}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	return windows.AssignProcessToJobObject(j.h, ph)
}

func (j *job) terminate() error {
	return windows.TerminateJobObject(j.h, 1)
}

func (j *job) close() {
	if j.h != 0 {
		windows.CloseHandle(j.h)
		j.h = 0
	}
}

type jobBasicAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// accounting returns job CPU time and active process count.
func (j *job) accounting() (cpuTime time.Duration, procs uint32, err error) {
	var acct jobBasicAccounting
	err = windows.QueryInformationJobObject(j.h, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil)
	if err != nil {
		return 0, 0, err
	}
	total := acct.TotalUserTime + acct.TotalKernelTime // 100ns units
	return time.Duration(total) * 100, acct.ActiveProcesses, nil
}

// processCreationTime returns a PID's creation time; PID reuse is detected
// by comparing it with the recorded value (§3.8).
func processCreationTime(pid int) (time.Time, bool) {
	ph, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return time.Time{}, false
	}
	defer windows.CloseHandle(ph)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(ph, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, creation.Nanoseconds()), true
}

func killProcess(pid int) error {
	ph, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(ph)
	return windows.TerminateProcess(ph, 1)
}
