//go:build !linux

package dnsserver

import "syscall"

const reuseportSupported = false

// reuseportControl is never reached off Linux: SocketCount returns 1 there.
func reuseportControl(_, _ string, _ syscall.RawConn) error { return nil }
