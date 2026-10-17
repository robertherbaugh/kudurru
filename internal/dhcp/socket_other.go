//go:build !linux

package dhcp

import (
	"errors"
	"net"
)

func listenDHCP(Config) (net.PacketConn, error) {
	return nil, errors.New("DHCP socket transport is supported only on Linux")
}
