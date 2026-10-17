//go:build unix

package dhcp

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// The registry ticket and lease row use separate SQLite commits. Keep this
// gate through lease COMMIT and publication so another scope cannot establish
// a later ticket while an earlier claim is still uncommitted.
func lockDNSClaims(ctx context.Context, path string) (*os.File, error) {
	f, err := privateLeaseFile(path, true)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
