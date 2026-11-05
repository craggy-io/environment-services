//go:build windows

package hostenv

import "syscall"

func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
