//go:build unix

package dnsdist

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Lock the directory inode, rather than a removable sidecar lock file. Every
// supervisor, including static mode, takes this lock before credentials,
// rendering, console access or child startup. Alias paths share ownership.
func acquireConfigState(path string, managed bool) (_ *configState, err error) {
	if err := os.MkdirAll(path, 0o750); err != nil { //nolint:gosec // operator-supplied ConfigDir, never network/request input
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("dnsdist: open configuration directory: %w", err)
	}
	dir := os.NewFile(uintptr(fd), path)
	defer func() {
		if err != nil {
			_ = dir.Close()
		}
	}()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("dnsdist: configuration directory already owned or cannot be locked: %w", err)
	}
	id, err := retainManagedBypass(fd, dir, managed)
	if err != nil {
		return nil, err
	}
	state := &configState{dir: dir, bypassID: id}
	if managed {
		var instance [32]byte
		if _, err := rand.Read(instance[:]); err != nil {
			return nil, err
		}
		if instance == ([32]byte{}) {
			return nil, errors.New("dnsdist: empty managed supervisor identity")
		}
		state.runID = hex.EncodeToString(instance[:])
	}
	return state, nil
}

func retainManagedBypass(dirFD int, dir *os.File, enabled bool) (string, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	fd, err := unix.Openat(dirFD, managedBypassFile, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		if !enabled {
			return "", nil
		}
		return createManagedBypass(dirFD, dir)
	}
	if err != nil {
		return "", fmt.Errorf("dnsdist: open retained managed cache policy: %w", err)
	}
	f := os.NewFile(uintptr(fd), managedBypassFile)
	defer func() { _ = f.Close() }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return "", err
	}
	const size = int64(len(managedBypassHeader) + 64 + 1)
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 ||
		int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&0o077 != 0 || stat.Size != size {
		return "", errors.New("dnsdist: retained managed cache policy must be an exact private regular file owned by the supervisor")
	}
	b, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return "", err
	}
	id, ok := strings.CutPrefix(string(b), managedBypassHeader)
	if !ok || len(id) != 65 || id[64] != '\n' {
		return "", errors.New("dnsdist: invalid retained managed cache policy")
	}
	id = id[:64]
	decoded, err := hex.DecodeString(id)
	if err != nil || hex.EncodeToString(decoded) != id || id == strings.Repeat("0", 64) {
		return "", errors.New("dnsdist: invalid retained managed cache identity")
	}
	if !enabled {
		return "", errors.New("dnsdist: this configuration directory requires managed cache bypass; refusing to reenable caches")
	}
	// A previous process may have stopped after writing complete bytes but
	// before directory sync. Reconfirm durability before admitting any child.
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := syncConfigPath(dir); err != nil {
		return "", err
	}
	return id, nil
}

func createManagedBypass(dirFD int, dir *os.File) (string, error) {
	var identity [32]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", err
	}
	if identity == ([32]byte{}) {
		return "", errors.New("dnsdist: empty managed cache identity")
	}
	id := hex.EncodeToString(identity[:])
	fd, err := unix.Openat(dirFD, managedBypassFile, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", fmt.Errorf("dnsdist: create retained managed cache policy: %w", err)
	}
	f := os.NewFile(uintptr(fd), managedBypassFile)
	defer func() { _ = f.Close() }()
	// Never remove or replace an incomplete file on failure. A crash or I/O
	// error must not turn a later static start into permission to attach caches.
	if _, err := io.WriteString(f, managedBypassHeader+id+"\n"); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := syncConfigPath(dir); err != nil {
		return "", err
	}
	return id, nil
}

// MkdirAll may have created several ancestors. Sync the containing directory
// and every parent so a successful first admission never acknowledges only a
// file whose directory entries have not been made durable. ConfigDir is trusted
// operator storage and must not be renamed, replaced or restored while in use.
func syncConfigPath(dir *os.File) error {
	if err := dir.Sync(); err != nil {
		return err
	}
	path, err := filepath.EvalSymlinks(dir.Name())
	if err != nil {
		return err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return err
	}
	for path != filepath.Dir(path) {
		path = filepath.Dir(path)
		parent, err := os.Open(path) //nolint:gosec // ancestor of the operator-owned configuration directory
		if err != nil {
			return err
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}
