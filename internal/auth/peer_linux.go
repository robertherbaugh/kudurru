//go:build linux

package auth

import (
	"fmt"
	"net"
	"syscall"
)

// ReadPeerCred returns the SO_PEERCRED credentials of a unix-socket
// connection.
func ReadPeerCred(c net.Conn) (PeerCred, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return PeerCred{}, ErrNoPeerCred
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return PeerCred{}, fmt.Errorf("auth: peer credentials: %w", err)
	}
	var (
		cred *syscall.Ucred
		gerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, gerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) // #nosec G115 -- a file descriptor fits in int
	}); err != nil {
		return PeerCred{}, fmt.Errorf("auth: peer credentials: %w", err)
	}
	if gerr != nil {
		return PeerCred{}, fmt.Errorf("auth: SO_PEERCRED: %w", gerr)
	}
	return PeerCred{PID: cred.Pid, UID: cred.Uid, GID: cred.Gid}, nil
}
