//go:build linux

package dnsserver

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const reuseportSupported = true

// reuseportControl sets SO_REUSEPORT before bind so several sockets can
// share one address and the kernel balances flows across them.
func reuseportControl(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1) // #nosec G115 -- a socket descriptor fits in int
	}); err != nil {
		return err
	}
	return serr
}
