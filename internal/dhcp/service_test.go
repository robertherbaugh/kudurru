package dhcp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func relayConfig() Config {
	cfg := protocolConfig()
	cfg.Name, cfg.Interface, cfg.RelayOnly = "remote", "transit0", true
	cfg.ServerIP = netip.MustParseAddr("198.51.100.2")
	cfg.TrustedRelays = []TrustedRelay{{SourceIP: netip.MustParseAddr("198.51.100.1"), GatewayIP: netip.MustParseAddr("192.0.2.254")}}
	return cfg
}

func serviceForTest(t *testing.T, configs ...Config) *Service {
	t.Helper()
	stores := map[string]*LeaseStore{}
	for _, cfg := range configs {
		name := normalizedConfig(cfg).Name
		store, err := OpenLeases(filepath.Join(t.TempDir(), name+".db"), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		stores[name] = store
	}
	svc, err := NewService(configs, stores, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

func TestServiceValidationAndSharedTransport(t *testing.T) {
	direct := protocolConfig()
	relay := relayConfig()
	relay.Interface, relay.ServerIP = direct.Interface, direct.ServerIP
	relay.Subnet = netip.MustParsePrefix("203.0.113.0/24")
	relay.RangeStart, relay.RangeEnd = netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.20")
	relay.Router = netip.MustParseAddr("203.0.113.254")
	relay.TrustedRelays[0].GatewayIP = relay.Router
	svc := serviceForTest(t, direct, relay)
	if len(svc.listeners) != 1 {
		t.Fatal("scopes on same interface did not share transport")
	}
	reversed, err := NewService([]Config{relay, direct}, svc.stores, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reversed.Close() }()
	if reversed.listeners[0].cfg.RelayOnly {
		t.Fatal("shared direct transport skipped Ethernet validation")
	}
	names := svc.ScopeNames()
	names[0] = "changed"
	if svc.ScopeNames()[0] != "default" {
		t.Fatal("ScopeNames aliases service")
	}
	if _, err := svc.Scope("missing"); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if _, err := NewService(nil, nil, nil); err == nil {
		t.Fatal("empty service accepted")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.Name = "default" },
		func(c *Config) {
			c.Subnet = direct.Subnet
			c.RangeStart = direct.RangeStart
			c.RangeEnd = direct.RangeEnd
			c.Router = direct.Router
		},
		func(c *Config) { c.ServerIP = netip.MustParseAddr("192.0.2.99") },
		func(c *Config) { c.RelayOnly = false; c.ServerIP = netip.MustParseAddr("203.0.113.1") },
	} {
		cfg := normalizedConfig(relay)
		edit(&cfg)
		// Existing stores deliberately have different topology for invalid edits.
		if _, err := NewService([]Config{direct, cfg}, svc.stores, nil); err == nil {
			t.Fatal("invalid topology accepted")
		}
	}
}

func TestServiceRoutingAndRelayRenewalLifecycle(t *testing.T) {
	cfg := relayConfig()
	cfg.Reservations = []Reservation{{MAC: "02:00:00:00:00:01", IP: netip.MustParseAddr("192.0.2.99")}}
	direct := protocolConfig()
	direct.Interface, direct.ServerIP = cfg.Interface, cfg.ServerIP
	direct.Subnet = netip.MustParsePrefix("198.51.100.0/24")
	direct.RangeStart, direct.RangeEnd = netip.MustParseAddr("198.51.100.10"), netip.MustParseAddr("198.51.100.20")
	direct.Router = netip.MustParseAddr("198.51.100.254")
	svc := serviceForTest(t, direct, cfg)
	ctx := context.Background()
	relayPeer := &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: serverPort}
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	packet.GatewayIPAddr, packet.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 1
	packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 3, 'l', 'a', 'n', 2, 0}))
	call := func(p *dhcpv4.DHCPv4, peer *net.UDPAddr) *dhcpv4.DHCPv4 {
		t.Helper()
		wire, destination, err := svc.dispatch(ctx, cfg.Interface, p.ToBytes(), peer)
		if err != nil {
			t.Fatal(err)
		}
		if wire == nil {
			return nil
		}
		reply, err := dhcpv4.FromBytes(wire)
		if err != nil {
			t.Fatal(err)
		}
		if p.GatewayIPAddr.To4() != nil && !p.GatewayIPAddr.IsUnspecified() && (!destination.IP.Equal(p.GatewayIPAddr) || destination.Port != serverPort || !reply.GatewayIPAddr.Equal(p.GatewayIPAddr)) {
			t.Fatal("incorrect relay delivery", destination)
		}
		if (p.GatewayIPAddr.To4() == nil || p.GatewayIPAddr.IsUnspecified()) && (!destination.IP.Equal(p.ClientIPAddr) || destination.Port != clientPort) {
			t.Fatal("incorrect routed renewal delivery", destination)
		}
		return reply
	}
	offer := call(packet, relayPeer)
	if offer == nil || !offer.YourIPAddr.Equal(net.IP{192, 0, 2, 99}) {
		t.Fatal("relay reservation offer missing")
	}
	packet.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
	packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
	packet.UpdateOption(dhcpv4.OptRequestedIPAddress(offer.YourIPAddr))
	if ack := call(packet, relayPeer); ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatal("relay DORA failed")
	}
	packet.GatewayIPAddr, packet.HopCount, packet.ClientIPAddr = nil, 0, offer.YourIPAddr
	packet.DeleteOption(dhcpv4.OptionRelayAgentInformation)
	packet.DeleteOption(dhcpv4.OptionServerIdentifier)
	packet.DeleteOption(dhcpv4.OptionRequestedIPAddress)
	if ack := call(packet, packetSource(packet)); ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatal("routed renewal failed")
	}
	packet.ClientHWAddr[5] = 2
	if call(packet, packetSource(packet)) != nil {
		t.Fatal("unknown routed renewal answered")
	}
	packet.ClientHWAddr[5] = 1
	packet.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRelease))
	packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
	if call(packet, packetSource(packet)) != nil {
		t.Fatal("release answered")
	}
	leases, err := svc.stores[cfg.Name].List(ctx)
	if err != nil || len(leases) != 0 {
		t.Fatal("release did not remove binding", leases, err)
	}
	// Option50 does not choose the remote scope on direct ingress.
	discover := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptRequestedIPAddress(offer.YourIPAddr))
	wire, _, err := svc.dispatch(ctx, cfg.Interface, discover.ToBytes(), packetSource(discover))
	if err != nil || wire == nil {
		t.Fatal("direct offer missing", err)
	}
	directOffer, _ := dhcpv4.FromBytes(wire)
	if !direct.Subnet.Contains(packetAddress(directOffer.YourIPAddr)) {
		t.Fatal("requested IP selected remote scope")
	}
}

func TestServiceAllOrNothingListenAndFailure(t *testing.T) {
	a := protocolConfig()
	b := relayConfig()
	b.Subnet = netip.MustParsePrefix("203.0.113.0/24")
	b.RangeStart, b.RangeEnd, b.Router = netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.1")
	b.TrustedRelays[0].GatewayIP = b.Router
	svc := serviceForTest(t, a, b)
	conn := newServerConn()
	injected := errors.New("bind failure")
	svc.listeners[0].listen = func(Config) (net.PacketConn, error) { return conn, nil }
	svc.listeners[1].listen = func(Config) (net.PacketConn, error) { return nil, injected }
	if err := svc.Listen(); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	select {
	case <-conn.closed:
	default:
		t.Fatal("partial startup left socket open")
	}
	svc = serviceForTest(t, a, b)
	conns := []*serverConn{newServerConn(), newServerConn()}
	for i, c := range conns {
		svc.listeners[i].listen = func(Config) (net.PacketConn, error) { return c, nil }
	}
	if err := svc.Run(context.Background()); err == nil {
		t.Fatal("Run before Listen succeeded")
	}
	if err := svc.Listen(); err != nil {
		t.Fatal(err)
	}
	conns[0].input <- serverDatagram{err: injected}
	if err := svc.Run(context.Background()); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	for _, c := range conns {
		select {
		case <-c.closed:
		default:
			t.Fatal("listener survived peer failure")
		}
	}
}

func TestServiceMutationSerializesAndCopiesCommittedOptions(t *testing.T) {
	cfg := protocolConfig()
	cfg.Management = "api"
	svc := serviceForTest(t, cfg)
	server := svc.scopes["default"]
	ctx := context.Background()
	started, release := make(chan struct{}), make(chan struct{})
	var committed atomic.Bool
	result := make(chan error, 1)
	go func() {
		result <- svc.MutateScope(ctx, "default", func(ctx context.Context, store *LeaseStore) error {
			close(started)
			<-release
			state := store.Configuration()
			options := optionsFor(state.Config)
			options.DomainName = "changed.example"
			err := store.UpdateOptions(ctx, state.Revision, options)
			committed.Store(err == nil)
			return err
		})
	}()
	<-started
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	response := make(chan []byte, 1)
	go func() {
		wire, _, _ := server.handlePacket(ctx, packet.ToBytes(), packetSource(packet))
		response <- wire
	}()
	select {
	case <-response:
		t.Fatal("packet interleaved with mutation")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	wire := <-response
	reply, err := dhcpv4.FromBytes(wire)
	if !committed.Load() || err != nil || reply.DomainName() != "changed.example" {
		t.Fatal("packet used stale config", err)
	}
	if err := svc.MutateScope(ctx, "default", func(_ context.Context, store *LeaseStore) error {
		state := store.Configuration()
		opts := optionsFor(state.Config)
		opts.DomainName = "committed.example"
		if err := store.UpdateOptions(ctx, state.Revision, opts); err != nil {
			return err
		}
		return errors.New("error after commit")
	}); err == nil {
		t.Fatal("callback error lost")
	}
	if server.cfg.DomainName != "committed.example" {
		t.Fatal("committed state not copied after callback error")
	}
}

func TestServiceRejectsConflictingEffectiveTopologies(t *testing.T) {
	a := protocolConfig()
	b := relayConfig()
	b.Interface, b.ServerIP = a.Interface, a.ServerIP
	b.Subnet = netip.MustParsePrefix("203.0.113.0/24")
	b.RangeStart, b.RangeEnd, b.Router = netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("203.0.113.1")
	b.TrustedRelays[0].GatewayIP = b.Router
	for _, tc := range []struct {
		name, want string
		configs    []Config
	}{
		{name: "overlapping subnet", want: "overlapping", configs: []Config{a, relayConfig()}},
		{name: "different IP same interface", want: "share server IP", configs: func() []Config {
			c := normalizedConfig(b)
			c.ServerIP = netip.MustParseAddr("192.0.2.99")
			return []Config{a, c}
		}()},
		{name: "ambiguous relay", want: "ambiguous", configs: func() []Config {
			c := normalizedConfig(b)
			c.Name = "other"
			c.Subnet = netip.MustParsePrefix("198.18.0.0/24")
			c.RangeStart, c.RangeEnd, c.Router = netip.MustParseAddr("198.18.0.10"), netip.MustParseAddr("198.18.0.20"), netip.MustParseAddr("198.18.0.1")
			return []Config{b, c}
		}()},
		{name: "another server allocatable", want: "another server address", configs: func() []Config {
			c := normalizedConfig(b)
			c.Interface = "other0"
			c.ServerIP = a.RangeStart
			return []Config{a, c}
		}()},
		{name: "foreign relay at mutable router", want: "relay gateway", configs: func() []Config {
			c := normalizedConfig(b)
			c.TrustedRelays[0].GatewayIP = a.Router
			return []Config{a, c}
		}()},
		{name: "foreign relay outside current pool", want: "relay gateway", configs: func() []Config {
			c := normalizedConfig(b)
			c.TrustedRelays[0].GatewayIP = netip.MustParseAddr("192.0.2.200")
			return []Config{a, c}
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stores := map[string]*LeaseStore{}
			for _, cfg := range tc.configs {
				name := normalizedConfig(cfg).Name
				store, err := OpenLeases(filepath.Join(t.TempDir(), name+".db"), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close() }()
				stores[name] = store
			}
			_, err := NewService(tc.configs, stores, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
