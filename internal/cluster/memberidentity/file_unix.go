//go:build unix

package memberidentity

import (
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The parent is operator-controlled; retain its descriptor while opening the
// final name so replacing the path cannot redirect creation into another parent.
func openFile(path string, create bool) (*os.File, *os.File, error) {
	dirfd, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, ErrIdentity
	}
	dir := os.NewFile(uintptr(dirfd), "member identity directory")
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	}
	fd, err := unix.Openat(dirfd, filepath.Base(path), flags, 0o600)
	if err != nil {
		_ = dir.Close()
		return nil, nil, ErrIdentity
	}
	f := os.NewFile(uintptr(fd), "member identity")
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Size > maxFileBytes {
		_ = f.Close()
		_ = dir.Close()
		return nil, nil, ErrIdentity
	}
	return f, dir, nil
}

func createFile(path string, raw []byte) error {
	f, dir, err := openFile(path, true)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	defer func() { _ = f.Close() }()
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		return ErrIdentity
	}
	if f.Sync() != nil || dir.Sync() != nil {
		return ErrIdentity
	}
	if f.Close() != nil {
		return ErrIdentity
	}
	return nil
}

func readFile(path string) ([]byte, error) {
	f, dir, err := openFile(path, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil || len(raw) > maxFileBytes {
		return nil, ErrIdentity
	}
	return raw, nil
}
