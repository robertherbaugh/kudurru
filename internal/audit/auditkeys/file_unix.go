//go:build unix

package auditkeys

import (
	"os"
	"syscall"
)

func openProtected(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) // #nosec G304 G703 -- explicit operator-selected protected file
	if err != nil {
		return nil, err
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0o600 || stat.Size() > MaxBytes {
		_ = f.Close()
		return nil, ErrFile
	}
	return f, nil
}
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600) // #nosec G304 G703 -- explicit operator-selected lock path
	if err != nil {
		return nil, ErrFile
	}
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0o600 {
		_ = f.Close()
		return nil, ErrFile
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrFile
	}
	return func() { _ = f.Close() }, nil
}
