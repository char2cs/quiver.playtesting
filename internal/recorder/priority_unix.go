//go:build unix

package recorder

import "syscall"

// lowerPriority makes the encoder yield to the gateway, which shares a small host.
func lowerPriority(pid int) { _ = syscall.Setpriority(syscall.PRIO_PROCESS, pid, 19) }
