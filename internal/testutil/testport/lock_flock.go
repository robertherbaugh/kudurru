//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package testport

import (
	"errors"
	"os"
	"syscall"
)

// lockPort takes an exclusive, non-blocking flock on path. The lock belongs
// to the open file, so it excludes other processes and other reservations
// in this process alike, and the kernel drops it if the process dies.
func lockPort(path string) (func(), bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the shared lock directory
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: file descriptors fit in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: file descriptors fit in int
		_ = f.Close()
	}, true, nil
}
