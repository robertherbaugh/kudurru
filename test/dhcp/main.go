//go:build linux

// This fixture is only for run.sh's disconnected network namespaces. It is not
// a deployment command. The relay is independently authored from RFC 1542/3046;
// the real DHCP client is an external BSD-licensed dhcpcd executable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func configs() []dhcp.Config {
	makeScope := func(name, iface, prefix, server, router, start, end string) dhcp.Config {
		return dhcp.Config{Name: name, Interface: iface, Subnet: netip.MustParsePrefix(prefix), ServerIP: addr(server), Router: addr(router), RangeStart: addr(start), RangeEnd: addr(end), DNSServers: []netip.Addr{addr(server)}, DomainName: name + ".example", LeaseDuration: time.Minute}
	}
	a := makeScope("default", "sa", "192.0.2.0/24", "192.0.2.1", "192.0.2.1", "192.0.2.10", "192.0.2.11")
	b := makeScope("second", "sb", "198.18.0.0/24", "198.18.0.1", "198.18.0.1", "198.18.0.10", "198.18.0.11")
	b.Reservations = []dhcp.Reservation{{MAC: "02:00:00:00:02:01", IP: addr("198.18.0.50"), Hostname: "reserved"}}
	transit := makeScope("transit", "st", "198.51.100.0/24", "198.51.100.2", "198.51.100.1", "198.51.100.100", "198.51.100.101")
	remote := makeScope("remote", "st", "203.0.113.0/24", "198.51.100.2", "203.0.113.1", "203.0.113.10", "203.0.113.11")
	remote.RelayOnly = true
	remote.TrustedRelays = []dhcp.TrustedRelay{{SourceIP: addr("198.51.100.1"), GatewayIP: addr("203.0.113.1")}}
	return []dhcp.Config{a, b, transit, remote}
}

func writeJSON(root *os.Root, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err = root.WriteFile("leases.json.tmp", data, 0600); err != nil {
		return err
	}
	return root.Rename("leases.json.tmp", "leases.json")
}

func serve(ctx context.Context, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	stores := map[string]*dhcp.LeaseStore{}
	for _, cfg := range configs() {
		filename := "dhcp-" + cfg.Name + ".db"
		if cfg.Name == "default" {
			filename = "dhcp.db"
		}
		store, err := dhcp.OpenLeases(filepath.Join(dir, filename), cfg)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		stores[cfg.Name] = store
	}
	service, err := dhcp.NewService(configs(), stores, nil)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	if err = service.Listen(); err != nil {
		return err
	}
	snapshot := func() error {
		state := map[string][]dhcp.Lease{}
		for name, store := range stores {
			leases, err := store.List(ctx)
			if err != nil {
				return err
			}
			state[name] = leases
		}
		return writeJSON(root, state)
	}
	if err = snapshot(); err != nil {
		return err
	}
	if err = root.WriteFile("ready", []byte("ready\n"), 0600); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- service.Run(ctx) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return err
		case <-ticker.C:
			if ctx.Err() == nil {
				if err = snapshot(); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return <-result
		}
	}
}

// relay uses only the private lab interfaces, with a fixed trusted giaddr and
// circuit ID. It intentionally supports only the lab's single downstream link.
func relay(ctx context.Context) error {
	up, err := net.InterfaceByName("up")
	if err != nil {
		return err
	}
	down, err := net.InterfaceByName("lan0")
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 67})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var sockErr error
	if err = raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1) }); err != nil {
		return err
	}
	if sockErr != nil {
		return sockErr
	}
	packet := ipv4.NewPacketConn(conn)
	if err = packet.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	buffer := make([]byte, 4097)
	for {
		n, cm, source, err := packet.ReadFrom(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if n > 4096 || cm == nil {
			continue
		}
		msg, err := dhcpv4.FromBytes(buffer[:n])
		if err != nil {
			continue
		}
		var destination *net.UDPAddr
		control := &ipv4.ControlMessage{}
		peer, ok := source.(*net.UDPAddr)
		if !ok {
			continue
		}
		switch msg.OpCode {
		case dhcpv4.OpcodeBootRequest:
			if cm.IfIndex != down.Index || peer.Port != 68 || msg.HopCount >= 16 {
				continue
			}
			msg.HopCount++
			if msg.GatewayIPAddr.IsUnspecified() {
				msg.GatewayIPAddr = net.IP{203, 0, 113, 1}
			}
			msg.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 3, 'l', 'a', 'b', 2, 2, 0, 1}))
			destination = &net.UDPAddr{IP: net.IP{198, 51, 100, 2}, Port: 67}
			control.Src, control.IfIndex = net.IP{198, 51, 100, 1}, up.Index
		case dhcpv4.OpcodeBootReply:
			if cm.IfIndex != up.Index || peer.Port != 67 || !peer.IP.Equal(net.IP{198, 51, 100, 2}) || !msg.GatewayIPAddr.Equal(net.IP{203, 0, 113, 1}) {
				continue
			}
			if got := msg.Options.Get(dhcpv4.OptionRelayAgentInformation); len(got) != 9 {
				return errors.New("relay option was not echoed")
			}
			msg.DeleteOption(dhcpv4.OptionRelayAgentInformation)
			destination = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
			control.Src, control.IfIndex = net.IP{203, 0, 113, 1}, down.Index
		default:
			continue
		}
		if _, err = packet.WriteTo(msg.ToBytes(), control, destination); err != nil {
			return err
		}
	}
}

func main() {
	// The shell proves namespace isolation before invoking this fixture. Require
	// its marker too so an accidental bare invocation fails before any sockets.
	currentNS, nsErr := os.Readlink("/proc/self/ns/net")
	hostNS := os.Getenv("KUDURRU_DHCP_HOST_NETNS")
	if nsErr != nil || hostNS == "" || currentNS == hostNS || os.Getenv("KUDURRU_DHCP_PRIVATE_LAB") != "1" {
		log.Fatal("run only through test/dhcp/run.sh")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if len(os.Args) < 2 {
		log.Fatal("expected server or relay")
	}
	var err error
	switch os.Args[1] {
	case "server":
		if len(os.Args) != 3 {
			log.Fatal("server requires state directory")
		}
		err = serve(ctx, os.Args[2])
	case "relay":
		err = relay(ctx)
	default:
		err = fmt.Errorf("unknown fixture mode %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
