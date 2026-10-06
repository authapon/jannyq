//go:build !unix

package sandbox

// HelperArg exists only so that callers compile on every platform.
const HelperArg = "sandbox-exec"

// HelperMain is unsupported without a Unix system.
func HelperMain([]string) int { return 126 }
