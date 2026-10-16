package dhcp

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func TestRelayExtensionsAuthorizedTuple(t *testing.T) {
	cfg := relayConfig()
	cfg.TrustedRelays[0].SourcePort = 1067
	cfg.TrustedRelays[0].LinkSelection = cfg.RangeStart
	cfg.TrustedRelays[0].ServerIdentifier = cfg.Router
	info := append([]byte{1, 1, 'x', 5, 4}, cfg.RangeStart.AsSlice()...)
	info = append(info, 11, 4)
	info = append(info, cfg.Router.AsSlice()...)
	info = append(info, 19, 0)
	packet := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, info))
	packet.GatewayIPAddr, packet.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 1
	peer := &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: 1067}
	backend := &protocolLeases{lease: Lease{Address: cfg.RangeStart}}
	server := &Server{cfg: cfg, leases: backend}
	wire, dest, err := server.handlePacket(context.Background(), packet.ToBytes(), peer)
	if err != nil || wire == nil {
		t.Fatal("authorized relay dropped", err)
	}
	reply, err := dhcpv4.FromBytes(wire)
	if err != nil || dest.Port != 1067 || !dest.IP.Equal(packet.GatewayIPAddr) || !reply.ServerIdentifier().Equal(net.IP(cfg.Router.AsSlice())) || !bytes.Equal(reply.Options.Get(dhcpv4.OptionRelayAgentInformation), info) {
		t.Fatal("relay extensions not honored", reply, dest, err)
	}
	packet.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
	packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP(cfg.RangeStart.AsSlice())))
	packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.Router.AsSlice())))
	wire, _, err = server.handlePacket(context.Background(), packet.ToBytes(), peer)
	if err != nil || wire == nil || backend.operation != "ack" {
		t.Fatal("overridden selecting request dropped", err)
	}
	for _, mutate := range []func(*dhcpv4.DHCPv4, *net.UDPAddr){
		func(_ *dhcpv4.DHCPv4, p *net.UDPAddr) { p.Port = 1068 },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) { p.Options[82] = append([]byte(nil), info[:len(info)-2]...) },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.Options[82] = append([]byte(nil), info...)
			p.Options[82][8]++
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) { p.Options[82] = append(append([]byte(nil), info...), 19, 0) },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.Options[82] = append(append([]byte(nil), info[:len(info)-2]...), 19, 2, 4, 43)
		},
	} {
		p := *packet
		p.Options = make(dhcpv4.Options)
		for code, value := range packet.Options {
			p.Options[code] = append([]byte(nil), value...)
		}
		source := *peer
		mutate(&p, &source)
		calls := backend.calls
		wire, _, err = server.handlePacket(context.Background(), p.ToBytes(), &source)
		if err != nil || wire != nil || backend.calls != calls {
			t.Fatal("unauthorized relay extension reached storage", err)
		}
	}
}

type probeLeases struct {
	protocolLeases
	candidates    []Lease
	quarantined   []netip.Addr
	quarantineErr error
}

func (p *probeLeases) Offer(_ context.Context, c Client, requested netip.Addr) (Lease, error) {
	p.record("offer", c, requested)
	if len(p.candidates) == 0 {
		return Lease{}, ErrNoAddress
	}
	lease := p.candidates[0]
	p.candidates = p.candidates[1:]
	return lease, nil
}
func (p *probeLeases) QuarantineOffer(_ context.Context, _ Client, address netip.Addr) error {
	p.quarantined = append(p.quarantined, address)
	return p.quarantineErr
}

func TestConflictProbeBeforeOffer(t *testing.T) {
	cfg := protocolConfig()
	cfg.ConflictDetection = "arp"
	first, second := cfg.RangeStart, cfg.RangeStart.Next()
	backend := &probeLeases{candidates: []Lease{{Address: first, State: "offered"}, {Address: second, State: "offered"}}}
	var probed []netip.Addr
	server := &Server{cfg: cfg, leases: backend, probe: func(_ context.Context, c Config, ip netip.Addr) (bool, error) {
		if c.ProbeTimeout != 200*time.Millisecond {
			t.Fatal("probe defaults missing")
		}
		probed = append(probed, ip)
		return ip == first, nil
	}}
	reply, _ := protocolReply(t, server, clientPacket(dhcpv4.MessageTypeDiscover))
	if reply == nil || !reply.YourIPAddr.Equal(net.IP(second.AsSlice())) || len(probed) != 2 || len(backend.quarantined) != 1 || backend.quarantined[0] != first {
		t.Fatal("conflict was offered or not quarantined")
	}
	backend = &probeLeases{candidates: []Lease{{Address: first, State: "bound"}}}
	server.leases = backend
	server.probe = func(context.Context, Config, netip.Addr) (bool, error) {
		t.Fatal("bound lease was probed")
		return false, nil
	}
	if reply, _ = protocolReply(t, server, clientPacket(dhcpv4.MessageTypeDiscover)); reply == nil {
		t.Fatal("bound rediscovery dropped")
	}
}

func TestConflictProbeFailuresAndBound(t *testing.T) {
	for _, failure := range []string{"socket", "quarantine", "all conflicts"} {
		t.Run(failure, func(t *testing.T) {
			cfg := protocolConfig()
			cfg.ConflictDetection = "icmp"
			backend := &probeLeases{candidates: []Lease{{Address: cfg.RangeStart, State: "offered"}, {Address: cfg.RangeStart.Next(), State: "offered"}, {Address: cfg.RangeStart.Next().Next(), State: "offered"}}}
			server := &Server{cfg: cfg, leases: backend, probe: func(context.Context, Config, netip.Addr) (bool, error) { return true, nil }}
			if failure == "socket" {
				server.probe = func(context.Context, Config, netip.Addr) (bool, error) { return false, errors.New("socket denied") }
			}
			if failure == "quarantine" {
				backend.quarantineErr = errors.New("disk failed")
			}
			wire, _, err := server.handlePacket(context.Background(), clientPacket(dhcpv4.MessageTypeDiscover).ToBytes(), &net.UDPAddr{IP: net.IPv4zero, Port: 68})
			if wire != nil || failure != "all conflicts" && err == nil || failure == "all conflicts" && (err != nil || backend.calls != 3 || len(backend.quarantined) != 3) {
				t.Fatal("probe failure not bounded or failed open", err, backend.calls)
			}
		})
	}
}

func TestOrdinaryDHCPOptionsAndBootProfiles(t *testing.T) {
	cfg := protocolConfig()
	cfg.NTPServers = []netip.Addr{netip.MustParseAddr("192.0.2.3")}
	cfg.DomainSearch = []string{"home.example", "lab.example"}
	cfg.ClasslessRoutes = []ClasslessRoute{{Destination: netip.MustParsePrefix("0.0.0.0/0"), Router: cfg.Router}, {Destination: netip.MustParsePrefix("10.20.0.0/16"), Router: cfg.Router}}
	cfg.BootFile = "legacy.efi"
	cfg.BootProfiles = []BootProfile{{Architecture: 7, BootServer: cfg.ServerIP, BootServerName: "boot.example", BootFile: "http://boot.example/boot.efi"}}
	backend := &protocolLeases{lease: Lease{Address: cfg.RangeStart}}
	server := &Server{cfg: cfg, leases: backend}
	fqdn := append([]byte{5, 0, 0}, encodeSearchDomains([]string{"client.home.example"})...)
	packet := clientPacket(dhcpv4.MessageTypeDiscover,
		dhcpv4.OptGeneric(dhcpv4.OptionParameterRequestList, []byte{121, 249, 42, 119, 66, 67}),
		dhcpv4.OptGeneric(dhcpv4.OptionClientSystemArchitectureType, []byte{0, 99, 0, 7}),
		dhcpv4.OptGeneric(dhcpv4.OptionFQDN, fqdn))
	reply, _ := protocolReply(t, server, packet)
	if reply == nil || reply.BootFileName != cfg.BootProfiles[0].BootFile || !reply.ServerIPAddr.Equal(net.IP(cfg.ServerIP.AsSlice())) {
		t.Fatal("boot profile not selected")
	}
	for code, want := range map[byte][]byte{42: cfg.NTPServers[0].AsSlice(), 119: encodeSearchDomains(cfg.DomainSearch), 121: encodeClasslessRoutes(cfg.ClasslessRoutes), 249: encodeClasslessRoutes(cfg.ClasslessRoutes), 66: []byte("boot.example"), 67: []byte(cfg.BootProfiles[0].BootFile), 81: append([]byte{14, 0, 0}, fqdn[3:]...)} {
		if !bytes.Equal(reply.Options[code], want) {
			t.Fatalf("option %d=%x, want %x", code, reply.Options[code], want)
		}
	}
	if backend.client.Hostname != "client.home.example" {
		t.Fatal("client FQDN not used as advisory hostname")
	}
}

func TestClientFQDNAndArchitectureMalformed(t *testing.T) {
	for _, value := range [][]byte{{}, {1}, {9, 0, 0}, {2, 0, 0}, {4, 0, 0, 192, 0}, {4, 0, 0, 3, 'a'}, {4, 0, 0, 1, 'x', 0, 1, 'z'}, {0, 0, 0, '-', 'x'}} {
		backend := &protocolLeases{lease: Lease{Address: protocolConfig().RangeStart}}
		server := &Server{cfg: protocolConfig(), leases: backend}
		p := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptGeneric(dhcpv4.OptionFQDN, value))
		wire, _, err := server.handlePacket(context.Background(), p.ToBytes(), packetSource(p))
		if err != nil || wire != nil || backend.calls != 0 {
			t.Fatal("malformed FQDN reached store", value, err)
		}
	}
	for _, value := range [][]byte{{}, {0}, {0, 7, 1}} {
		p := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptGeneric(dhcpv4.OptionClientSystemArchitectureType, value))
		if decodeClientPacket(p.ToBytes(), packetSource(p)) != nil {
			t.Fatal("malformed architecture accepted")
		}
	}
}

type servingLeases struct {
	protocolLeases
	denied bool
	cfg    Config
}

func (b *servingLeases) CheckServing(context.Context) error {
	if b.denied {
		return errors.New("no quorum")
	}
	return nil
}
func (b *servingLeases) Configuration() ManagementState { return ManagementState{Config: b.cfg} }

func TestServingGuardAndCommittedOptionsRefresh(t *testing.T) {
	cfg := protocolConfig()
	backend := &servingLeases{protocolLeases: protocolLeases{lease: Lease{Address: cfg.RangeStart}}, denied: true, cfg: cfg}
	server := &Server{cfg: cfg, leases: backend}
	for _, message := range []dhcpv4.MessageType{dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeInform, dhcpv4.MessageTypeRequest} {
		p := clientPacket(message)
		if message == dhcpv4.MessageTypeInform {
			p.ClientIPAddr = net.IP(cfg.RangeStart.AsSlice())
		}
		wire, _, err := server.handlePacket(context.Background(), p.ToBytes(), packetSource(p))
		if err == nil || wire != nil || backend.calls != 0 {
			t.Fatal("non-serving backend replied or mutated", message, err)
		}
	}
	backend.denied = false
	backend.cfg.NTPServers = []netip.Addr{cfg.Router}
	packet := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptGeneric(dhcpv4.OptionParameterRequestList, []byte{42}))
	reply, _ := protocolReply(t, server, packet)
	if reply == nil || !bytes.Equal(reply.Options.Get(dhcpv4.OptionNTPServers), cfg.Router.AsSlice()) {
		t.Fatal("replicated committed options not refreshed")
	}
}

type changedOptionsLeases struct {
	servingLeases
	grant time.Duration
}

func (b *changedOptionsLeases) Acknowledge(_ context.Context, c Client, address netip.Addr, _ RequestMode) (Lease, error) {
	// A committed remote config change can race the initial packet snapshot.
	b.cfg.LeaseDuration = b.grant
	b.record("ack", c, address)
	return Lease{Address: address, State: "bound", GrantedDuration: b.grant}, nil
}

func TestReplyUsesCommittedGrantAfterRemoteConfigChange(t *testing.T) {
	cfg := protocolConfig()
	backend := &changedOptionsLeases{servingLeases: servingLeases{cfg: cfg}, grant: 10 * time.Minute}
	server := &Server{cfg: cfg, leases: backend}
	packet := clientPacket(dhcpv4.MessageTypeRequest, dhcpv4.OptRequestedIPAddress(net.IP(cfg.RangeStart.AsSlice())), dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
	reply, _ := protocolReply(t, server, packet)
	if reply == nil || reply.IPAddressLeaseTime(-1) != 10*time.Minute || reply.IPAddressRenewalTime(-1) != 5*time.Minute || reply.IPAddressRebindingTime(-1) != 525*time.Second {
		t.Fatal("wire duration exceeded committed lease grant", reply)
	}
}
