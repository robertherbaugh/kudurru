//go:build !(linux || darwin || freebsd || openbsd || netbsd || dragonfly)

package testport

// lockPort has no cross-process lock on this platform: reservations are
// exclusive within the process only (the in-process table in reserve).
func lockPort(string) (func(), bool, error) {
	return func() {}, true, nil
}
