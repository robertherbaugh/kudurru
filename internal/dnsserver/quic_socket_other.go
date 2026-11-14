//go:build !linux

package dnsserver

import (
	"errors"
	"net"
)

func attachQUICRouting(net.PacketConn, int) error {
	return errors.New("dnsserver: QUIC multi-socket routing requires Linux")
}
