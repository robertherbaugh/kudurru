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
	a := makeScope("default", "sa", "192.0.2.0/24", "192.0.2.1", "192.0.2.1", "192.0.2.9", "192.0.2.11")
	a.NTPServers = []netip.Addr{a.ServerIP}
	a.DomainSearch = []string{"default.example", "lab.example"}
	a.ClasslessRoutes = []dhcp.ClasslessRoute{{Destination: netip.MustParsePrefix("0.0.0.0/0"), Router: a.Router}, {Destination: netip.MustParsePrefix("10.123.0.0/16"), Router: a.Router}}
	a.BootServer, a.BootServerName, a.BootFile = a.ServerIP, "boot.default.example", "netboot.efi"
	b := makeScope("second", "sb", "198.18.0.0/24", "198.18.0.1", "198.18.0.1", "198.18.0.10", "198.18.0.11")
	b.Reservations = []dhcp.Reservation{{MAC: "02:00:00:00:02:01", IP: addr("198.18.0.50"), Hostname: "reserved"}}
	transit := makeScope("transit", "st", "198.51.100.0/24", "198.51.100.2", "198.51.100.1", "198.51.100.100", "198.51.100.101")
	// The same source/giaddr can authorize distinct links. This mapping also
	// excludes immutable relay/override infrastructure from the transit pool.
	transit.TrustedRelays = []dhcp.TrustedRelay{{SourceIP: addr("198.51.100.1"), GatewayIP: addr("198.51.100.1"), SourcePort: 1067, LinkSelection: addr("198.51.100.1"), ServerIdentifier: addr("198.51.100.3")}}
	remote := makeScope("remote", "st", "203.0.113.0/24", "198.51.100.2", "203.0.113.1", "203.0.113.9", "203.0.113.11")
	remote.RelayOnly = true
	remote.TrustedRelays = []dhcp.TrustedRelay{{SourceIP: addr("198.51.100.1"), GatewayIP: addr("198.51.100.1"), SourcePort: 1067, LinkSelection: addr("203.0.113.1"), ServerIdentifier: addr("198.51.100.3")}}
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
	if err := checkCapabilities(); err != nil {
		return err
	}
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
	uplink, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 1067})
	if err != nil {
		return err
	}
	defer func() { _ = uplink.Close() }()
	upstream := ipv4.NewPacketConn(uplink)
	if err = upstream.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	packet := ipv4.NewPacketConn(conn)
	if err = packet.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); _ = uplink.Close() })
	defer stop()
	type received struct {
		wire   []byte
		cm     *ipv4.ControlMessage
		source net.Addr
		err    error
	}
	incoming := make(chan received, 2)
	for _, socket := range []*ipv4.PacketConn{packet, upstream} {
		go func() {
			buffer := make([]byte, 4097)
			for {
				n, cm, source, err := socket.ReadFrom(buffer)
				select {
				case incoming <- received{append([]byte(nil), buffer[:max(n, 0)]...), cm, source, err}:
				case <-ctx.Done():
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	for {
		var event received
		select {
		case event = <-incoming:
		case <-ctx.Done():
			return nil
		}
		buffer, cm, source, err := event.wire, event.cm, event.source, event.err
		n := len(buffer)
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
				msg.GatewayIPAddr = net.IP{198, 51, 100, 1}
			}
			msg.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 3, 'l', 'a', 'b', 2, 2, 0, 1, 5, 4, 203, 0, 113, 1, 11, 4, 198, 51, 100, 3, 19, 0}))
			destination = &net.UDPAddr{IP: net.IP{198, 51, 100, 2}, Port: 67}
			control.Src, control.IfIndex = net.IP{198, 51, 100, 1}, up.Index
		case dhcpv4.OpcodeBootReply:
			if cm.IfIndex != up.Index || peer.Port != 67 || !peer.IP.Equal(net.IP{198, 51, 100, 2}) || !msg.GatewayIPAddr.Equal(net.IP{198, 51, 100, 1}) {
				continue
			}
			if got := msg.Options.Get(dhcpv4.OptionRelayAgentInformation); len(got) != 23 {
				return errors.New("relay option was not echoed")
			}
			msg.DeleteOption(dhcpv4.OptionRelayAgentInformation)
			destination = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
			control.Src, control.IfIndex = net.IP{203, 0, 113, 1}, down.Index
		default:
			continue
		}
		output := packet
		if msg.OpCode == dhcpv4.OpcodeBootRequest {
			output = upstream
		}
		if _, err = output.WriteTo(msg.ToBytes(), control, destination); err != nil {
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
	case "capabilities":
		err = checkCapabilities()
		if err == nil {
			fmt.Println("PASS uid65532, exact effective/permitted/bounding capabilities, no-new-privileges, AF_PACKET socket")
		}
	default:
		err = fmt.Errorf("unknown fixture mode %q", os.Args[1])
	}
	if err != nil && (ctx.Err() == nil || !errors.Is(err, ctx.Err())) {
		log.Fatal(err)
	}
}
