//go:build linux

package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// interfacePacketConn pins the source address as well as the output interface.
// Binding only to the server IP would miss limited broadcasts sent by clients
// without an address, so the receive socket binds INADDR_ANY instead.
type interfacePacketConn struct {
	*net.UDPConn
	packet  *ipv4.PacketConn
	control ipv4.ControlMessage
}

func (c *interfacePacketConn) WriteTo(b []byte, destination net.Addr) (int, error) {
	return c.packet.WriteTo(b, &c.control, destination)
}

func listenDHCP(cfg Config) (net.PacketConn, error) {
	iface, err := net.InterfaceByName(cfg.Interface)
	if err != nil {
		return nil, err
	}
	if err := validateDHCPInterface(*iface, cfg.RelayOnly); err != nil {
		return nil, err
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("interface addresses: %w", err)
	}
	found := false
	for _, address := range addresses {
		if prefix, err := netip.ParsePrefix(address.String()); err == nil && prefix.Addr().Unmap() == cfg.ServerIP {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("server IP %s is not assigned to interface %s", cfg.ServerIP, cfg.Interface)
	}
	return listenInterfaceUDP(cfg.Interface, fmt.Sprintf(":%d", serverPort), net.IP(cfg.ServerIP.AsSlice()), iface.Index)
}

// No SO_REUSEADDR or SO_REUSEPORT: a competing DHCP listener must make startup
// fail, even when it enabled reuse itself. SO_BINDTODEVICE confines reception
// and limited broadcasts to the selected LAN.
func listenInterfaceUDP(ifname, address string, source net.IP, ifindex int) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		err := raw.Control(func(fd uintptr) {
			socketErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifname) // #nosec G115 -- socket descriptors fit int
			if socketErr == nil {
				socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1) // #nosec G115 -- socket descriptors fit int
			}
		})
		return errors.Join(err, socketErr)
	}}
	conn, err := lc.ListenPacket(context.Background(), "udp4", address)
	if err != nil {
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("expected a UDP socket")
	}
	return &interfacePacketConn{
		UDPConn: udp,
		packet:  ipv4.NewPacketConn(udp),
		control: ipv4.ControlMessage{Src: source, IfIndex: ifindex},
	}, nil
}

// Relay-only transports need no Ethernet or broadcast delivery. A listener
// shared with any direct scope still requires a broadcast-capable Ethernet LAN.
func validateDHCPInterface(iface net.Interface, relayOnly bool) error {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return errors.New("configured interface must be active and must not be loopback")
	}
	if !relayOnly && (iface.Flags&net.FlagBroadcast == 0 || len(iface.HardwareAddr) != 6) {
		return errors.New("direct-client interface must be an Ethernet LAN with broadcast support")
	}
	return nil
}
