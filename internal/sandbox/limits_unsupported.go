//go:build unix && !linux && !darwin

package sandbox

import "errors"

const rlimitNproc = 0

// Other Unix systems are not supported yet; the helper then refuses to start
// commands rather than run them without limits.
func setrlimit(int, uint64, uint64) error {
	return errors.New("resource limits are not implemented on this platform")
}
