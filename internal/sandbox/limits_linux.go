//go:build linux

package sandbox

import "syscall"

const rlimitNproc = 6 // RLIMIT_NPROC (not exported by package syscall on every platform)

func setrlimit(resource int, soft, hard uint64) error {
	return syscall.Setrlimit(resource, &syscall.Rlimit{Cur: soft, Max: hard})
}
