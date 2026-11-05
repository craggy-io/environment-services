//go:build !windows

package hostenv_test

import "os"

func processAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_, err = os.Stat("/proc/" + itoa(pid))
	return err == nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
