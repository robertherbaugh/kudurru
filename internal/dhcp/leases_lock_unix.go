//go:build unix

package dhcp

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Lock the database inode itself so alternate paths cannot create two owners.
// SQLite's POSIX record locks are independent of this flock.
func lockLeaseFile(path string) (*os.File, error) {
	f, err := privateLeaseFile(path, true)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("dhcp: lease database is already owned: %w", err)
	}
	return f, nil
}

func privateLeaseFile(path string, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("dhcp: open lease file: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || int64(st.Uid) != int64(os.Geteuid()) || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("dhcp: lease file must be a regular file owned exclusively by the current user")
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readOnlyLeaseFile checks private storage without chmod or taking the owner's
// flock. SQLite's read transaction supplies a consistent metadata/lease cut.
func readOnlyLeaseFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = f.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int64(stat.Uid) != int64(os.Geteuid()) || stat.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("dhcp: lease file must be a regular file owned exclusively by the current user")
	}
	return f, nil
}
