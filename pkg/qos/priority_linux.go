//go:build linux

package qos

import (
	"syscall"
)

// SetProcessNicePriority sets the CPU scheduling priority (nice value -20 to 19)
func SetProcessNicePriority(priority int) error {
	// 19 is lowest priority, yields to NAS DSM, streaming, and user tasks
	if priority < -20 {
		priority = -20
	}
	if priority > 19 {
		priority = 19
	}
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, priority)
}
