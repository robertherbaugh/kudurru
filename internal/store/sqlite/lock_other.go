//go:build !unix

package sqlite

// fileLock is a no-op where flock(2) is unavailable; the operator must not
// run two processes on one state.db there.
type fileLock struct{}

func acquireLock(string) (*fileLock, error) { return &fileLock{}, nil }

func (l *fileLock) release() error { return nil }
