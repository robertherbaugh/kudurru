//go:build unix

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// fileLock is an advisory flock(2) on <state.db>.lock that keeps a second
// process from opening the same state.db (two writers would each believe
// they are the leader and the in-process broadcaster would miss the other
// one's writes).
type fileLock struct{ f *os.File }

func acquireLock(path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- derived from the operator-supplied data path
	if err != nil {
		return nil, fmt.Errorf("store/sqlite: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- fds fit in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock %s)", ErrLocked, path)
		}
		return nil, fmt.Errorf("store/sqlite: lock %s: %w", path, err)
	}
	return &fileLock{f: f}, nil
}

func (l *fileLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) // #nosec G115 -- fds fit in int
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
