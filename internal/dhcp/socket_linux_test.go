//go:build linux

package dhcp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestSocketLoopbackDeliveryAndOptions(t *testing.T) {
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := listenInterfaceUDP("lo", "0.0.0.0:0", net.IPv4(127, 0, 0, 2), iface.Index)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
		t.Skip("SO_BINDTODEVICE requires NET_RAW on this host")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	bound := conn.(*interfacePacketConn)
	raw, err := bound.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Control(func(fd uintptr) {
		device, err := unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
		if err != nil || device != "lo" {
			t.Errorf("bound device %q: %v", device, err)
		}
		for option, want := range map[int]int{unix.SO_BROADCAST: 1, unix.SO_REUSEADDR: 0, unix.SO_REUSEPORT: 0} {
			value, err := unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, option)
			if err != nil || value != want {
				t.Errorf("socket option %d = %d, want %d: %v", option, value, want, err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	serverAddress := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: conn.LocalAddr().(*net.UDPAddr).Port}
	if _, err := client.WriteTo([]byte("loopback request"), serverAddress); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 128)
	n, peer, err := conn.ReadFrom(buffer)
	if err != nil || string(buffer[:n]) != "loopback request" {
		t.Fatalf("read: %q %v", buffer[:n], err)
	}
	if _, err := conn.WriteTo([]byte("loopback reply"), peer); err != nil {
		t.Fatal(err)
	}
	n, peer, err = client.ReadFrom(buffer)
	expectedSource := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: serverAddress.Port}
	if err != nil || string(buffer[:n]) != "loopback reply" || peer.String() != expectedSource.String() {
		t.Fatalf("source-pinned reply: %q from %v: %v", buffer[:n], peer, err)
	}
}

func TestSocketRefusesCompetingListener(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		lc := net.ListenConfig{}
		if reuse {
			lc.Control = func(_, _ string, raw syscall.RawConn) error {
				var socketErr error
				err := raw.Control(func(fd uintptr) {
					socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
					if socketErr == nil {
						socketErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
					}
				})
				return errors.Join(err, socketErr)
			}
		}
		existing, err := lc.ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		conn, err := listenInterfaceUDP("", existing.LocalAddr().String(), net.IPv4(127, 0, 0, 1), 0)
		_ = existing.Close()
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, unix.EADDRINUSE) {
			t.Fatalf("competing listener (reuse=%t) did not fail exclusively: %v", reuse, err)
		}
	}
}

func TestSocketInterfaceValidation(t *testing.T) {
	cfg := protocolConfig()
	cfg.Interface = "dhcp-not-found"
	if conn, err := listenDHCP(cfg); err == nil {
		_ = conn.Close()
		t.Fatal("nonexistent interface accepted")
	}
	cfg.Interface = "lo"
	if conn, err := listenDHCP(cfg); err == nil {
		_ = conn.Close()
		t.Fatal("loopback interface accepted as Ethernet LAN")
	}
	// Wrong-IP startup validation runs before any port 67 bind.
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp != 0 && iface.Flags&net.FlagBroadcast != 0 && len(iface.HardwareAddr) == 6 {
			cfg.Interface = iface.Name
			// 0.0.0.0 cannot be a configured interface address.
			cfg.ServerIP = netip.IPv4Unspecified()
			if conn, err := listenDHCP(cfg); err == nil {
				_ = conn.Close()
				t.Fatal("unassigned server IP accepted")
			}
			return
		}
	}
}

func TestRelayOnlyInterfaceDoesNotRequireEthernet(t *testing.T) {
	iface := net.Interface{Name: "private-tunnel", Flags: net.FlagUp | net.FlagPointToPoint}
	if err := validateDHCPInterface(iface, true); err != nil {
		t.Fatal("relay-only tunnel rejected", err)
	}
	if err := validateDHCPInterface(iface, false); err == nil {
		t.Fatal("direct client accepted without Ethernet broadcast")
	}
	iface.Flags |= net.FlagLoopback
	if err := validateDHCPInterface(iface, true); err == nil {
		t.Fatal("loopback accepted")
	}
	iface.Flags = 0
	if err := validateDHCPInterface(iface, true); err == nil {
		t.Fatal("down relay interface accepted")
	}
}
