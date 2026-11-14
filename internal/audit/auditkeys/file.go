package auditkeys

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ErrFile deliberately omits private contents and underlying filesystem errors.
var ErrFile = errors.New("audit keys: protected file operation failed (requires a regular 0600 file and writable private directory)")

// UpdateFile serializes cooperating operator commands, reads a bounded protected
// file (or requires absence for init), and durably replaces it. The containing
// directory is operator-controlled; the persistent lock file must not be removed.
func UpdateFile(ctx context.Context, path string, initialize bool, update func(*Ring) (*Ring, error)) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	unlock, err := lockFile(path + ".lock")
	if err != nil {
		return 0, ErrFile
	}
	defer unlock()
	var ring *Ring
	f, err := openProtected(path)
	if initialize {
		if f != nil {
			_ = f.Close()
		}
		if !errors.Is(err, os.ErrNotExist) {
			return 0, ErrFile
		}
	} else {
		if err != nil {
			return 0, ErrFile
		}
		data, readErr := io.ReadAll(io.LimitReader(f, MaxBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return 0, ErrFile
		}
		ring, err = Parse(data)
		if err != nil {
			return 0, err
		}
	}
	next, err := update(ring)
	if err != nil {
		return 0, err
	}
	data, err := next.EncodePrivate()
	if err != nil || len(data) > MaxBytes {
		return 0, ErrUnavailable
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".audit-keys-*")
	if err != nil {
		return 0, ErrFile
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return 0, ErrFile
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, ErrFile
	}
	if err = tmp.Close(); err != nil {
		return 0, ErrFile
	}
	if err = ctx.Err(); err != nil {
		return 0, err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return 0, ErrFile
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return 0, ErrFile
	}
	err = dir.Sync()
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return 0, ErrFile
	}
	return next.Count(), nil
}

// ReadProtected bounds an explicit legacy master-key import file as well.
func ReadProtected(path string) ([]byte, error) {
	f, err := openProtected(path)
	if err != nil {
		return nil, ErrFile
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(data) > MaxBytes {
		return nil, ErrFile
	}
	return data, nil
}
