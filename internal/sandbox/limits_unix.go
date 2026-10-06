//go:build unix

package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// HelperArg is the first argument with which the executor re-runs its own
// binary to start a command: `jannyq sandbox-exec CPU NOFILE FSIZE NPROC SHELL COMMAND`.
// The helper applies the resource limits with setrlimit(2) and then replaces
// itself with the shell, so limits never depend on the shell's own `ulimit`
// (dash, for one, has no `ulimit -u`). It is internal: the executor calls it.
const HelperArg = "sandbox-exec"

// HelperMain implements the helper. args are the arguments after HelperArg.
// It only returns on failure, with the process exit code.
func HelperMain(args []string) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "sandbox: "+format+"\n", a...)
		return code
	}
	if len(args) != 6 {
		return fail(126, "internal error: bad helper arguments")
	}
	var n [4]uint64
	for i := range n {
		v, err := strconv.ParseUint(args[i], 10, 63)
		if err != nil {
			return fail(126, "internal error: bad limit %q", args[i])
		}
		n[i] = v
	}
	cpu, nofile, fsize, nproc := n[0], n[1], n[2], n[3]
	limits := []struct {
		name     string
		resource int
		soft     uint64
		hard     uint64
	}{
		{"core size", syscall.RLIMIT_CORE, 0, 0},
		// the hard CPU limit is a little higher so the process first gets SIGXCPU
		{"CPU time", syscall.RLIMIT_CPU, cpu, cpu + 5},
		{"open files", syscall.RLIMIT_NOFILE, nofile, nofile},
		{"file size", syscall.RLIMIT_FSIZE, fsize, fsize},
		{"processes", rlimitNproc, nproc, nproc},
	}
	for _, l := range limits {
		if err := setrlimit(l.resource, l.soft, l.hard); err != nil {
			return fail(126, "cannot apply the %s limit: %v", l.name, err)
		}
	}
	shell, command := args[4], args[5]
	err := syscall.Exec(shell, []string{shell, "-c", command}, os.Environ())
	return fail(127, "cannot start %s: %v", shell, err)
}
