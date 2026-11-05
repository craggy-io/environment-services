package androidemu

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

func TestIsEmulatorCommand(t *testing.T) {
	cases := []struct {
		name, cmdline string
		port          int
		want          bool
	}{
		{`C:\sdk\emulator\emulator.exe`, `emulator.exe -avd a -port 5554 -grpc 8554`, 5554, true},
		{"qemu-system-x86_64-headless.exe", "qemu-system-x86_64-headless.exe -avd a -port 5556", 5556, true},
		{"emulator", "", 5554, true}, // command line unreadable: name decides
		{"emulator.exe", "emulator.exe -avd a -port 5556", 5554, false},
		{"emulator.exe", "emulator.exe -avd a -port 55540", 5554, false},
		{"emulator.exe", "emulator -kill 37808 -sleep 20", 5554, false},
		{"notepad.exe", "notepad.exe -port 5554", 5554, false},
	}
	for _, c := range cases {
		if got := isEmulatorCommand(c.name, c.cmdline, c.port); got != c.want {
			t.Errorf("isEmulatorCommand(%q, %q, %d) = %v, want %v", c.name, c.cmdline, c.port, got, c.want)
		}
	}
}

// A cache entry whose PID now belongs to another program is dropped
// without touching that program (the DB Ref stays the authority;
// the stale-cache case is covered in androidemu_test.go).

// killTree must take down grandchildren too: QEMU is a child of
// emulator.exe and survives a kill of the parent alone.
func TestKillTreeKillsChildren(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping -n 60 127.0.0.1 >nul & exit")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 60; true")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a process tree: %v", err)
	}
	go func() { _ = cmd.Wait() }()
	root, err := process.NewProcess(int32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	var children []*process.Process
	deadline := time.Now().Add(5 * time.Second)
	for len(children) == 0 && time.Now().Before(deadline) {
		children, _ = root.Children()
		time.Sleep(50 * time.Millisecond)
	}
	if len(children) == 0 {
		_ = root.Kill()
		t.Skip("child process never appeared")
	}
	killTree(root)
	for _, c := range children {
		deadline := time.Now().Add(5 * time.Second)
		for {
			running, err := c.IsRunning()
			if err != nil || !running {
				break
			}
			if time.Now().After(deadline) {
				_ = c.Kill()
				t.Fatalf("child %d survived killTree", c.Pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
