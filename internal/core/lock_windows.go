//go:build windows

package core

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// acquireLock takes the single-controller lock via an OS file lock, so
// a crashed manager never leaves a stale lock.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	ol := &windows.Overlapped{}
	if err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, fmt.Errorf("core: another manager owns %s (single controller per Dir): %w", path, err)
	}
	return f, nil
}

func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	ol := &windows.Overlapped{}
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
	f.Close()
}
