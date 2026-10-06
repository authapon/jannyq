//go:build darwin

package sandbox

import "syscall"

const rlimitNproc = 7 // RLIMIT_NPROC on macOS

func setrlimit(resource int, soft, hard uint64) error {
	return syscall.Setrlimit(resource, &syscall.Rlimit{Cur: soft, Max: hard})
}
