package dhcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

func protocolConfig() Config {
	return Config{
		Interface: "eth0", ServerIP: netip.MustParseAddr("192.0.2.1"),
		Subnet:     netip.MustParsePrefix("192.0.2.0/24"),
		RangeStart: netip.MustParseAddr("192.0.2.10"), RangeEnd: netip.MustParseAddr("192.0.2.20"),
		Router: netip.MustParseAddr("192.0.2.254"), DNSServers: []netip.Addr{netip.MustParseAddr("192.0.2.1")},
		DomainName: "home.example", LeaseDuration: time.Hour,
	}
}

func clientPacket(message dhcpv4.MessageType, options ...dhcpv4.Option) *dhcpv4.DHCPv4 {
	packet := &dhcpv4.DHCPv4{
		OpCode: dhcpv4.OpcodeBootRequest, HWType: iana.HWTypeEthernet,
		TransactionID: dhcpv4.TransactionID{1, 2, 3, 4},
		ClientHWAddr:  net.HardwareAddr{2, 0, 0, 0, 0, 1},
		Options:       dhcpv4.OptionsFromList(dhcpv4.OptMessageType(message)),
	}
	for _, option := range options {
		packet.UpdateOption(option)
	}
	return packet
}

func packetSource(packet *dhcpv4.DHCPv4) *net.UDPAddr {
	ip := packet.ClientIPAddr
	if len(ip) == 0 {
		ip = net.IPv4zero
	}
	return &net.UDPAddr{IP: ip, Port: clientPort}
}

func protocolReply(t *testing.T, server *Server, packet *dhcpv4.DHCPv4) (*dhcpv4.DHCPv4, *net.UDPAddr) {
	t.Helper()
	wire, destination, err := server.handlePacket(context.Background(), packet.ToBytes(), packetSource(packet))
	if err != nil {
		t.Fatal(err)
	}
	if wire == nil {
		return nil, destination
	}
	reply, err := dhcpv4.FromBytes(wire)
	if err != nil {
		t.Fatal(err)
	}
	if reply.TransactionID != packet.TransactionID || !bytes.Equal(reply.ClientHWAddr, packet.ClientHWAddr) ||
		reply.OpCode != dhcpv4.OpcodeBootReply || !reply.ServerIdentifier().Equal(net.IP(server.cfg.ServerIP.AsSlice())) || destination.Port != clientPort {
		t.Fatalf("invalid reply envelope: %v, destination %v", reply, destination)
	}
	return reply, destination
}

type protocolLeases struct {
	client    Client
	address   netip.Addr
	mode      RequestMode
	operation string
	lease     Lease
	err       error
	calls     int
}

func (s *protocolLeases) record(operation string, client Client, address netip.Addr) {
	s.calls++
	s.operation, s.client, s.address = operation, client, address
}
func (s *protocolLeases) Offer(_ context.Context, client Client, address netip.Addr) (Lease, error) {
	s.record("offer", client, address)
	return s.lease, s.err
}
func (s *protocolLeases) Acknowledge(_ context.Context, client Client, address netip.Addr, mode RequestMode) (Lease, error) {
	s.record("ack", client, address)
	s.mode = mode
	return s.lease, s.err
}
func (s *protocolLeases) Release(_ context.Context, client Client, address netip.Addr) error {
	s.record("release", client, address)
	return s.err
}
func (s *protocolLeases) Decline(_ context.Context, client Client, address netip.Addr) error {
	s.record("decline", client, address)
	return s.err
}

func TestProtocolDiscoverAndRequestModes(t *testing.T) {
	cfg := protocolConfig()
	address := net.IP{192, 0, 2, 10}
	for _, tc := range []struct {
		name      string
		message   dhcpv4.MessageType
		current   net.IP
		options   []dhcpv4.Option
		mode      RequestMode
		broadcast bool
	}{
		{"discover", dhcpv4.MessageTypeDiscover, nil, nil, Selecting, true},
		{"discover requested", dhcpv4.MessageTypeDiscover, nil, []dhcpv4.Option{dhcpv4.OptRequestedIPAddress(address)}, Selecting, true},
		{"selecting", dhcpv4.MessageTypeRequest, nil, []dhcpv4.Option{dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())), dhcpv4.OptRequestedIPAddress(address)}, Selecting, true},
		{"init reboot", dhcpv4.MessageTypeRequest, nil, []dhcpv4.Option{dhcpv4.OptRequestedIPAddress(address)}, InitReboot, true},
		{"renew", dhcpv4.MessageTypeRequest, address, nil, Renewing, false},
		{"rebind", dhcpv4.MessageTypeRequest, address, nil, Renewing, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
			server := &Server{cfg: cfg, leases: backend}
			packet := clientPacket(tc.message, tc.options...)
			packet.ClientIPAddr = tc.current
			packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{0, 255, 0, 42}))
			packet.UpdateOption(dhcpv4.OptHostName("client.example"))
			if tc.name == "rebind" {
				packet.SetBroadcast()
			}
			reply, destination := protocolReply(t, server, packet)
			if reply == nil || !reply.YourIPAddr.Equal(address) {
				t.Fatalf("missing or incorrect reply: %v", reply)
			}
			expectedType := dhcpv4.MessageTypeAck
			if tc.message == dhcpv4.MessageTypeDiscover {
				expectedType = dhcpv4.MessageTypeOffer
			} else if backend.mode != tc.mode || backend.address != netip.MustParseAddr("192.0.2.10") {
				t.Fatalf("request mode/address: %v %v", backend.mode, backend.address)
			}
			if reply.MessageType() != expectedType || backend.calls != 1 || backend.client.ID != "00ff002a" || backend.client.MAC != "02:00:00:00:00:01" || backend.client.Hostname != "client.example" {
				t.Fatalf("wrong response/client: %v %#v", reply, backend.client)
			}
			if tc.broadcast != destination.IP.Equal(net.IPv4bcast) || !tc.broadcast && !destination.IP.Equal(address) {
				t.Fatalf("incorrect delivery: %v", destination)
			}
			for option, want := range map[byte][]byte{
				1: {255, 255, 255, 0}, 3: {192, 0, 2, 254}, 6: {192, 0, 2, 1}, 15: []byte("home.example"),
				51: {0, 0, 14, 16}, 58: {0, 0, 7, 8}, 59: {0, 0, 12, 78}, 61: {0, 255, 0, 42},
			} {
				if !bytes.Equal(reply.Options[option], want) {
					t.Errorf("option %d = %v, want %v", option, reply.Options[option], want)
				}
			}
			if !reply.ServerIPAddr.IsUnspecified() || !reply.GatewayIPAddr.IsUnspecified() {
				t.Fatal("boot server or relay address unexpectedly set")
			}
		})
	}
}

func TestProtocolLeaseErrors(t *testing.T) {
	fatal := errors.New("storage failed")
	for _, tc := range []struct {
		name    string
		mode    RequestMode
		message dhcpv4.MessageType
		err     error
		want    dhcpv4.MessageType
		fatal   bool
	}{
		{"exhausted", Selecting, dhcpv4.MessageTypeDiscover, ErrNoAddress, 0, false},
		{"unavailable offer", Selecting, dhcpv4.MessageTypeDiscover, ErrUnavailable, 0, false},
		{"select unknown", Selecting, dhcpv4.MessageTypeRequest, ErrUnknownLease, dhcpv4.MessageTypeNak, false},
		{"init unknown", InitReboot, dhcpv4.MessageTypeRequest, ErrUnknownLease, 0, false},
		{"renew unknown", Renewing, dhcpv4.MessageTypeRequest, ErrUnknownLease, 0, false},
		{"init wrong address", InitReboot, dhcpv4.MessageTypeRequest, ErrUnavailable, dhcpv4.MessageTypeNak, false},
		{"renew wrong address", Renewing, dhcpv4.MessageTypeRequest, ErrUnavailable, dhcpv4.MessageTypeNak, false},
		{"storage failure", Selecting, dhcpv4.MessageTypeDiscover, fatal, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &protocolLeases{err: tc.err}
			server := &Server{cfg: protocolConfig(), leases: backend}
			packet := clientPacket(tc.message, dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{1, 2}))
			if tc.message == dhcpv4.MessageTypeRequest {
				if tc.mode == Renewing {
					packet.ClientIPAddr = net.IP{192, 0, 2, 10}
				} else {
					packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
				}
				if tc.mode == Selecting {
					packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
				}
			}
			wire, destination, err := server.handlePacket(context.Background(), packet.ToBytes(), packetSource(packet))
			if tc.fatal != errors.Is(err, fatal) {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want == 0 {
				if wire != nil || destination != nil {
					t.Fatal("unexpected reply")
				}
				return
			}
			reply, err := dhcpv4.FromBytes(wire)
			if err != nil || reply.MessageType() != tc.want || !reply.IsBroadcast() || !destination.IP.Equal(net.IPv4bcast) || !reply.ClientIPAddr.IsUnspecified() || !reply.YourIPAddr.IsUnspecified() {
				t.Fatalf("invalid NAK: %v %v %v", reply, destination, err)
			}
			if reply.Options.Has(dhcpv4.OptionIPAddressLeaseTime) || reply.Options.Has(dhcpv4.OptionSubnetMask) {
				t.Fatal("NAK contains lease or network configuration")
			}
			if !bytes.Equal(reply.Options.Get(dhcpv4.OptionClientIdentifier), []byte{1, 2}) {
				t.Fatal("NAK did not echo client identifier")
			}
		})
	}
}

func TestProtocolInform(t *testing.T) {
	backend := &protocolLeases{}
	server := &Server{cfg: protocolConfig(), leases: backend}
	packet := clientPacket(dhcpv4.MessageTypeInform, dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{0, 42}), dhcpv4.OptParameterRequestList(dhcpv4.OptionSubnetMask, dhcpv4.OptionDomainNameServer, dhcpv4.OptionDomainName))
	packet.ClientIPAddr = net.IP{192, 0, 2, 80}
	packet.SetBroadcast()
	reply, destination := protocolReply(t, server, packet)
	if reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck || !reply.YourIPAddr.IsUnspecified() || !destination.IP.Equal(packet.ClientIPAddr) || backend.calls != 0 {
		t.Fatalf("invalid INFORM reply: %v %v, backend calls %d", reply, destination, backend.calls)
	}
	for _, option := range []dhcpv4.OptionCode{dhcpv4.OptionIPAddressLeaseTime, dhcpv4.OptionRenewTimeValue, dhcpv4.OptionRebindingTimeValue, dhcpv4.OptionRouter} {
		if reply.Options.Has(option) {
			t.Errorf("unexpected INFORM option %d", option.Code())
		}
	}
	for _, option := range []dhcpv4.OptionCode{dhcpv4.OptionSubnetMask, dhcpv4.OptionDomainNameServer, dhcpv4.OptionDomainName} {
		if !reply.Options.Has(option) {
			t.Errorf("missing INFORM option %d", option.Code())
		}
	}
	if !bytes.Equal(reply.Options.Get(dhcpv4.OptionClientIdentifier), []byte{0, 42}) {
		t.Fatal("INFORM response did not echo client identifier")
	}
}

func TestProtocolReleaseDeclineNoReplies(t *testing.T) {
	for _, message := range []dhcpv4.MessageType{dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline} {
		for _, leaseErr := range []error{nil, ErrUnknownLease, ErrUnavailable} {
			backend := &protocolLeases{err: leaseErr}
			server := &Server{cfg: protocolConfig(), leases: backend}
			packet := clientPacket(message, dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
			if message == dhcpv4.MessageTypeRelease {
				packet.ClientIPAddr = net.IP{192, 0, 2, 10}
			} else {
				packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
			}
			if reply, _ := protocolReply(t, server, packet); reply != nil || backend.calls != 1 || backend.address != netip.MustParseAddr("192.0.2.10") {
				t.Fatalf("mutation response/call: %v %#v", reply, backend)
			}
		}
	}
}

func TestProtocolStrictValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*dhcpv4.DHCPv4)
	}{
		{"server response", func(p *dhcpv4.DHCPv4) { p.OpCode = dhcpv4.OpcodeBootReply }},
		{"non Ethernet", func(p *dhcpv4.DHCPv4) { p.HWType = 0 }},
		{"short MAC", func(p *dhcpv4.DHCPv4) { p.ClientHWAddr = p.ClientHWAddr[:5] }},
		{"multicast MAC", func(p *dhcpv4.DHCPv4) { p.ClientHWAddr[0] = 1 }},
		{"zero MAC", func(p *dhcpv4.DHCPv4) { p.ClientHWAddr = make(net.HardwareAddr, 6) }},
		{"relay address", func(p *dhcpv4.DHCPv4) { p.GatewayIPAddr = net.IP{192, 0, 2, 2} }},
		{"relay hops", func(p *dhcpv4.DHCPv4) { p.HopCount = 1 }},
		{"relay option", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 1, 0}))
		}},
		{"yiaddr", func(p *dhcpv4.DHCPv4) { p.YourIPAddr = net.IP{192, 0, 2, 10} }},
		{"siaddr", func(p *dhcpv4.DHCPv4) { p.ServerIPAddr = net.IP{192, 0, 2, 1} }},
		{"missing type", func(p *dhcpv4.DHCPv4) { p.DeleteOption(dhcpv4.OptionDHCPMessageType) }},
		{"duplicate type", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionDHCPMessageType, []byte{1, 1})) }},
		{"invalid type", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptMessageType(99)) }},
		{"short requested address", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRequestedIPAddress, []byte{1})) }},
		{"zero requested address", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IPv4zero)) }},
		{"multicast requested address", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP{224, 0, 0, 1})) }},
		{"bad server ID", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, []byte{1})) }},
		{"bad lease time", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionIPAddressLeaseTime, []byte{1})) }},
		{"bad max size", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptMaxMessageSize(575)) }},
		{"bad max size length", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionMaximumDHCPMessageSize, []byte{1}))
		}},
		{"short client ID", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{1})) }},
		{"long client ID", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, make([]byte, 256)))
		}},
		{"empty parameter list", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionParameterRequestList, []byte{})) }},
		{"hostname control", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptHostName("bad\nname")) }},
		{"hostname invalid", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptHostName("bad name")) }},
		{"DISCOVER ciaddr", func(p *dhcpv4.DHCPv4) { p.ClientIPAddr = net.IP{192, 0, 2, 10} }},
		{"DISCOVER server", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1})) }},
		{"REQUEST missing address", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest)) }},
		{"REQUEST ciaddr plus requested", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
			p.ClientIPAddr = net.IP{192, 0, 2, 10}
			p.UpdateOption(dhcpv4.OptRequestedIPAddress(p.ClientIPAddr))
		}},
		{"REQUEST ciaddr plus server", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
			p.ClientIPAddr = net.IP{192, 0, 2, 10}
			p.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
		}},
		{"INFORM missing ciaddr", func(p *dhcpv4.DHCPv4) { p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeInform)) }},
		{"INFORM foreign subnet", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeInform))
			p.ClientIPAddr = net.IP{198, 51, 100, 10}
		}},
		{"RELEASE missing server", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRelease))
			p.ClientIPAddr = net.IP{192, 0, 2, 10}
		}},
		{"DECLINE missing address", func(p *dhcpv4.DHCPv4) {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeDecline))
			p.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 1}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &protocolLeases{}
			server := &Server{cfg: protocolConfig(), leases: backend}
			packet := clientPacket(dhcpv4.MessageTypeDiscover)
			tc.change(packet)
			reply, _ := protocolReply(t, server, packet)
			if reply != nil || backend.calls != 0 {
				t.Fatalf("invalid packet processed: reply %v, calls %d", reply, backend.calls)
			}
		})
	}
}

func TestProtocolOtherServerIgnored(t *testing.T) {
	for _, message := range []dhcpv4.MessageType{dhcpv4.MessageTypeRequest, dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline} {
		backend := &protocolLeases{}
		server := &Server{cfg: protocolConfig(), leases: backend}
		packet := clientPacket(message, dhcpv4.OptServerIdentifier(net.IP{192, 0, 2, 2}), dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
		if message == dhcpv4.MessageTypeRelease {
			packet.DeleteOption(dhcpv4.OptionRequestedIPAddress)
			packet.ClientIPAddr = net.IP{192, 0, 2, 10}
		}
		if reply, _ := protocolReply(t, server, packet); reply != nil || backend.calls != 0 {
			t.Fatalf("other server message %v processed", message)
		}
	}
}

func TestProtocolWireValidationAndOverload(t *testing.T) {
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	for _, tc := range []struct {
		name   string
		wire   []byte
		source *net.UDPAddr
	}{
		{"truncated", packet.ToBytes()[:239], packetSource(packet)},
		{"oversize", make([]byte, maxPacketSize+1), packetSource(packet)},
		{"wrong port", packet.ToBytes(), &net.UDPAddr{IP: net.IPv4zero, Port: 67}},
		{"IPv6 source", packet.ToBytes(), &net.UDPAddr{IP: net.IPv6loopback, Port: 68}},
		{"nil source", packet.ToBytes(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if decodeClientPacket(tc.wire, tc.source) != nil {
				t.Fatal("invalid wire packet accepted")
			}
		})
	}
	badCookie := packet.ToBytes()
	badCookie[236] = 0
	badHWLength := packet.ToBytes()
	badHWLength[2] = 255
	for _, wire := range [][]byte{badCookie, badHWLength, append(packet.ToBytes()[:240], 53, 4, 1)} {
		if decodeClientPacket(wire, packetSource(packet)) != nil {
			t.Fatal("malformed packet accepted")
		}
	}
	packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionOptionOverload, []byte{3}))
	packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{1}))
	wire := packet.ToBytes()
	copy(wire[108:236], []byte{61, 1, 2, 255})
	copy(wire[44:108], []byte{61, 1, 3, 255})
	decoded := decodeClientPacket(wire, packetSource(packet))
	if decoded == nil || !bytes.Equal(decoded.Options.Get(dhcpv4.OptionClientIdentifier), []byte{1, 2, 3}) {
		t.Fatal("option overload concatenation failed")
	}
	copy(wire[108:236], make([]byte, 128))
	if decodeClientPacket(wire, packetSource(packet)) != nil {
		t.Fatal("unterminated overload accepted")
	}
	packet = clientPacket(dhcpv4.MessageTypeRequest)
	packet.ClientIPAddr = net.IP{192, 0, 2, 10}
	if decodeClientPacket(packet.ToBytes(), &net.UDPAddr{IP: net.IP{192, 0, 2, 11}, Port: clientPort}) != nil {
		t.Fatal("mismatched source and ciaddr accepted")
	}
}

func TestProtocolResponseSize(t *testing.T) {
	server := &Server{cfg: protocolConfig(), leases: &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}}
	server.cfg.DomainName = string(bytes.Repeat([]byte("x"), 200))
	for _, limit := range []uint16{576, 1500} {
		packet := clientPacket(dhcpv4.MessageTypeDiscover,
			dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, bytes.Repeat([]byte{1}, 255)), dhcpv4.OptMaxMessageSize(limit))
		reply, _ := protocolReply(t, server, packet)
		if len(reply.ToBytes()) > int(limit) || len(reply.Options.Get(dhcpv4.OptionClientIdentifier)) != 255 || binary.BigEndian.Uint32(reply.Options.Get(dhcpv4.OptionIPAddressLeaseTime)) != 3600 {
			t.Fatal("size bound or required options lost")
		}
		if (limit == 1500) != reply.Options.Has(dhcpv4.OptionDomainName) {
			t.Fatal("optional parameters did not respect maximum message size")
		}
	}
}

func TestProtocolDurableLeaseExchange(t *testing.T) {
	cfg := protocolConfig()
	store, err := OpenLeases(filepath.Join(t.TempDir(), "dhcp.db"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server, err := NewServer(cfg, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	offer, _ := protocolReply(t, server, packet)
	if offer == nil {
		t.Fatal("no OFFER")
	}
	leases, err := store.List(context.Background())
	if err != nil || len(leases) != 1 || leases[0].State != "offered" || leases[0].ClientID != packet.ClientHWAddr.String() {
		t.Fatalf("OFFER was not persisted with MAC identity: %v %v", leases, err)
	}
	packet = clientPacket(dhcpv4.MessageTypeRequest, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())), dhcpv4.OptRequestedIPAddress(offer.YourIPAddr))
	ack, _ := protocolReply(t, server, packet)
	if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatal("no ACK")
	}
	leases, err = store.List(context.Background())
	if err != nil || len(leases) != 1 || leases[0].State != "bound" {
		t.Fatalf("ACK was not persisted: %v %v", leases, err)
	}
	packet = clientPacket(dhcpv4.MessageTypeRelease, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
	packet.ClientIPAddr = ack.YourIPAddr
	if reply, _ := protocolReply(t, server, packet); reply != nil {
		t.Fatal("RELEASE sent a reply")
	}
	leases, err = store.List(context.Background())
	if err != nil || len(leases) != 0 {
		t.Fatalf("RELEASE did not remove binding: %v %v", leases, err)
	}
}

func TestProtocolRestartRenewalReservationAndDecline(t *testing.T) {
	cfg := protocolConfig()
	reserved := netip.MustParseAddr("192.0.2.40")
	cfg.Reservations = []Reservation{{MAC: "02:00:00:00:00:09", IP: reserved, Hostname: "printer"}}
	path := filepath.Join(t.TempDir(), "dhcp.db")
	var store *LeaseStore
	var server *Server
	open := func() {
		t.Helper()
		var err error
		store, err = OpenLeases(path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		server, err = NewServer(cfg, store, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	restart := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		open()
	}
	open()
	t.Cleanup(func() { _ = store.Close() })
	unknown := clientPacket(dhcpv4.MessageTypeRequest, dhcpv4.OptRequestedIPAddress(net.IP{198, 51, 100, 10}))
	if reply, _ := protocolReply(t, server, unknown); reply != nil {
		t.Fatal("unknown INIT-REBOOT outside our pool received a reply")
	}
	id := dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, []byte{0, 1, 2})
	discover := clientPacket(dhcpv4.MessageTypeDiscover, id)
	offer, _ := protocolReply(t, server, discover)
	if offer == nil {
		t.Fatal("dynamic DISCOVER received no OFFER")
	}
	request := clientPacket(dhcpv4.MessageTypeRequest, id, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())), dhcpv4.OptRequestedIPAddress(offer.YourIPAddr))
	ack, _ := protocolReply(t, server, request)
	if ack == nil || ack.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatal("dynamic REQUEST received no ACK")
	}
	restart()
	initReboot := clientPacket(dhcpv4.MessageTypeRequest, id, dhcpv4.OptRequestedIPAddress(ack.YourIPAddr))
	if reply, destination := protocolReply(t, server, initReboot); reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck || !destination.IP.Equal(net.IPv4bcast) {
		t.Fatal("persisted binding could not INIT-REBOOT")
	}
	renew := clientPacket(dhcpv4.MessageTypeRequest, id)
	renew.ClientIPAddr = ack.YourIPAddr
	if reply, destination := protocolReply(t, server, renew); reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck || !destination.IP.Equal(ack.YourIPAddr) {
		t.Fatal("persisted binding could not renew by unicast")
	}
	reservedPacket := func(message dhcpv4.MessageType, options ...dhcpv4.Option) *dhcpv4.DHCPv4 {
		packet := clientPacket(message, options...)
		packet.ClientHWAddr[5] = 9
		return packet
	}
	discover = reservedPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptHostName("client-provided"))
	offer, _ = protocolReply(t, server, discover)
	if offer == nil || packetAddress(offer.YourIPAddr) != reserved {
		t.Fatalf("reservation outside pool was not offered: %v", offer)
	}
	request = reservedPacket(dhcpv4.MessageTypeRequest, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())), dhcpv4.OptRequestedIPAddress(offer.YourIPAddr))
	if reply, _ := protocolReply(t, server, request); reply == nil || reply.MessageType() != dhcpv4.MessageTypeAck {
		t.Fatal("reservation not acknowledged")
	}
	leases, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, lease := range leases {
		if lease.Address == reserved {
			found = lease.State == "bound" && lease.Hostname == "printer"
		}
	}
	if !found {
		t.Fatal("reservation hostname/binding not persisted")
	}
	release := reservedPacket(dhcpv4.MessageTypeRelease, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
	release.ClientIPAddr = net.IP(reserved.AsSlice())
	if reply, _ := protocolReply(t, server, release); reply != nil {
		t.Fatal("RELEASE received a reply")
	}
	if reply, _ := protocolReply(t, server, discover); reply == nil || packetAddress(reply.YourIPAddr) != reserved {
		t.Fatal("released reservation was not offered again")
	}
	decline := reservedPacket(dhcpv4.MessageTypeDecline, dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())), dhcpv4.OptRequestedIPAddress(net.IP(reserved.AsSlice())))
	if reply, _ := protocolReply(t, server, decline); reply != nil {
		t.Fatal("DECLINE received a reply")
	}
	restart()
	leases, err = store.List(context.Background())
	if err != nil || len(leases) != 2 {
		t.Fatalf("restart lost lease state: %v %v", leases, err)
	}
	found = false
	for _, lease := range leases {
		if lease.Address == reserved {
			found = lease.State == "declined" && lease.ExpiresAt.After(time.Now())
		} else if lease.ClientID != "000102" || lease.State != "bound" {
			t.Fatalf("restart lost dynamic client identity: %#v", lease)
		}
	}
	if !found {
		t.Fatal("decline quarantine did not survive restart")
	}
	if reply, _ := protocolReply(t, server, discover); reply != nil {
		t.Fatal("quarantined reservation was offered")
	}
}

func TestProtocolOptionOrder(t *testing.T) {
	server := &Server{cfg: protocolConfig(), leases: &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}}
	packet := clientPacket(dhcpv4.MessageTypeDiscover,
		dhcpv4.OptParameterRequestList(dhcpv4.OptionRouter, dhcpv4.OptionSubnetMask, dhcpv4.OptionDomainNameServer))
	wire, _, err := server.handlePacket(context.Background(), packet.ToBytes(), packetSource(packet))
	if err != nil {
		t.Fatal(err)
	}
	maskPosition, routerPosition, dnsPosition := -1, -1, -1
	for offset := 240; offset < len(wire) && wire[offset] != 255; {
		code := wire[offset]
		if code == 0 {
			offset++
			continue
		}
		if code == 1 {
			maskPosition = offset
		}
		if code == 3 {
			routerPosition = offset
		}
		if code == 6 {
			dnsPosition = offset
		}
		offset += 2 + int(wire[offset+1])
	}
	if maskPosition != 240 || routerPosition < maskPosition || dnsPosition < routerPosition {
		t.Fatalf("mask precedence and requested order: mask=%d router=%d DNS=%d", maskPosition, routerPosition, dnsPosition)
	}
	packet.UpdateOption(dhcpv4.OptParameterRequestList(dhcpv4.OptionDomainName, dhcpv4.OptionDomainNameServer))
	wire, _, err = server.handlePacket(context.Background(), packet.ToBytes(), packetSource(packet))
	if err != nil || wire[240] != 15 || wire[242+int(wire[241])] != 6 {
		t.Fatalf("requested domain/DNS order not honored: %v", err)
	}
}

func FuzzDHCPClientPacket(f *testing.F) {
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	f.Add(packet.ToBytes())
	f.Add([]byte{})
	f.Add([]byte("not DHCP"))
	f.Add(packet.ToBytes()[:239])
	f.Add(make([]byte, maxPacketSize+1))
	packet.UpdateOption(dhcpv4.OptHostName("bad\nname"))
	f.Add(packet.ToBytes())
	packet.DeleteOption(dhcpv4.OptionHostName)
	packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionOptionOverload, []byte{3}))
	f.Add(packet.ToBytes())
	packet = clientPacket(dhcpv4.MessageTypeRequest, dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
	f.Add(packet.ToBytes())
	packet.ClientIPAddr = net.IP{192, 0, 2, 10}
	f.Add(packet.ToBytes())
	f.Fuzz(func(t *testing.T, wire []byte) {
		backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
		server := &Server{cfg: protocolConfig(), leases: backend}
		source := &net.UDPAddr{IP: net.IPv4zero, Port: clientPort}
		if len(wire) >= 16 {
			source.IP = net.IP(wire[12:16])
		}
		reply, _, err := server.handlePacket(context.Background(), wire, source)
		if err != nil {
			t.Fatalf("packet input produced a service-fatal error: %v", err)
		}
		if len(reply) > maxPacketSize || backend.calls > 1 {
			t.Fatal("packet processing exceeded bounds")
		}
		if reply != nil {
			if _, err := dhcpv4.FromBytes(reply); err != nil {
				t.Fatalf("invalid encoded reply: %v", err)
			}
		}
	})
}

func TestProtocolTrustedRelayEnvelopeAndOptions(t *testing.T) {
	cfg := relayConfig()
	peer := &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: serverPort}
	relayInfo := []byte{1, 3, 'l', 'a', 'n', 2, 0}
	makePacket := func() *dhcpv4.DHCPv4 {
		p := clientPacket(dhcpv4.MessageTypeDiscover, dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, relayInfo), dhcpv4.OptClientIdentifier([]byte{1, 2, 3}), dhcpv4.OptParameterRequestList(dhcpv4.OptionRelayAgentInformation, dhcpv4.OptionRouter, dhcpv4.OptionDomainName, dhcpv4.OptionDomainNameServer))
		p.GatewayIPAddr, p.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 1
		return p
	}
	for _, responseType := range []dhcpv4.MessageType{dhcpv4.MessageTypeOffer, dhcpv4.MessageTypeNak} {
		backend := &protocolLeases{lease: Lease{Address: netip.MustParseAddr("192.0.2.10")}}
		server := &Server{cfg: cfg, leases: backend}
		p := makePacket()
		if responseType == dhcpv4.MessageTypeNak {
			p.UpdateOption(dhcpv4.OptMessageType(dhcpv4.MessageTypeRequest))
			p.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
			p.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
			backend.err = ErrUnavailable
		}
		wire, dest, err := server.handlePacket(context.Background(), p.ToBytes(), peer)
		if err != nil || wire == nil {
			t.Fatal("relay reply missing", err)
		}
		reply, err := dhcpv4.FromBytes(wire)
		if err != nil || reply.MessageType() != responseType || !reply.GatewayIPAddr.Equal(p.GatewayIPAddr) || !dest.IP.Equal(p.GatewayIPAddr) || dest.Port != serverPort {
			t.Fatal("incorrect relay response", reply, dest, err)
		}
		if !bytes.Equal(reply.Options.Get(dhcpv4.OptionRelayAgentInformation), relayInfo) || !bytes.Equal(reply.Options.Get(dhcpv4.OptionClientIdentifier), []byte{1, 2, 3}) {
			t.Fatal("relay information/client ID not echoed")
		}
		var codes []byte
		for offset := 240; wire[offset] != 255; {
			code := wire[offset]
			if code == 0 {
				offset++
				continue
			}
			codes = append(codes, code)
			offset += 2 + int(wire[offset+1])
		}
		if codes[len(codes)-1] != 82 {
			t.Fatal("option82 not last", codes)
		}
		if responseType == dhcpv4.MessageTypeNak && !reply.IsBroadcast() {
			t.Fatal("relayed NAK broadcast flag absent")
		}
	}
	for _, edit := range []func(*dhcpv4.DHCPv4, *net.UDPAddr){
		func(_ *dhcpv4.DHCPv4, a *net.UDPAddr) { a.IP = net.IP{198, 51, 100, 99} },
		func(_ *dhcpv4.DHCPv4, a *net.UDPAddr) { a.Port = clientPort },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) { p.GatewayIPAddr = net.IP{192, 0, 2, 253} },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) { p.HopCount = 0 },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) { p.HopCount = 17 },
		func(p *dhcpv4.DHCPv4, a *net.UDPAddr) { p.GatewayIPAddr = nil; p.HopCount = 0; a.Port = clientPort },
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1}))
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 4, 1}))
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{5, 4, 192, 0, 2, 1}))
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{11, 4, 192, 0, 2, 1}))
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{19, 0}))
		},
		func(p *dhcpv4.DHCPv4, _ *net.UDPAddr) {
			p.UpdateOption(dhcpv4.OptServerIdentifier(net.IP{198, 51, 100, 88}))
		},
	} {
		p := makePacket()
		a := *peer
		edit(p, &a)
		backend := &protocolLeases{lease: Lease{Address: cfg.RangeStart}}
		server := &Server{cfg: cfg, leases: backend}
		wire, _, err := server.handlePacket(context.Background(), p.ToBytes(), &a)
		if err != nil || wire != nil || backend.calls != 0 {
			t.Fatal("untrusted relay reached allocator", err)
		}
	}
}

func TestProtocolOversizedRelayEchoOmittedWhole(t *testing.T) {
	cfg := relayConfig()
	p := clientPacket(dhcpv4.MessageTypeDiscover)
	p.GatewayIPAddr, p.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 1
	// Mandatory client ID plus the entire relay option exceed the 576-byte limit.
	relayInfo := append([]byte{1, 253}, bytes.Repeat([]byte{7}, 253)...)
	p.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, relayInfo))
	p.UpdateOption(dhcpv4.OptClientIdentifier(bytes.Repeat([]byte{1}, 255)))
	backend := &protocolLeases{lease: Lease{Address: cfg.RangeStart}}
	server := &Server{cfg: cfg, leases: backend}
	wire, _, err := server.handlePacket(context.Background(), p.ToBytes(), &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: serverPort})
	if err != nil || len(wire) > 576 {
		t.Fatal("relay reply exceeds client limit", len(wire), err)
	}
	reply, err := dhcpv4.FromBytes(wire)
	if err != nil || reply.Options.Has(dhcpv4.OptionRelayAgentInformation) || len(reply.Options.Get(dhcpv4.OptionClientIdentifier)) != 255 {
		t.Fatal("relay echo was truncated or client ID lost", err)
	}
}

func FuzzDHCPRelayPacket(f *testing.F) {
	cfg := relayConfig()
	packet := clientPacket(dhcpv4.MessageTypeDiscover)
	packet.GatewayIPAddr, packet.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 1
	for _, info := range [][]byte{nil, {1}, {1, 0}, {1, 3, 1}, {1, 1, 1, 2, 0}, {5, 4, 192, 0, 2, 1}, {19, 0}} {
		packet.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, info))
		f.Add(packet.ToBytes())
	}
	f.Fuzz(func(t *testing.T, wire []byte) {
		backend := &protocolLeases{lease: Lease{Address: cfg.RangeStart}}
		server := &Server{cfg: cfg, leases: backend}
		reply, _, err := server.handlePacket(context.Background(), wire, &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: serverPort})
		if err != nil || len(reply) > maxPacketSize || backend.calls > 1 {
			t.Fatal("packet input caused error or exceeded bounds", err)
		}
	})
}

func TestProtocolConfiguredClientOnWrongDirectSubnet(t *testing.T) {
	cfg := protocolConfig()
	packet := clientPacket(dhcpv4.MessageTypeRequest)
	packet.ClientIPAddr = net.IP{203, 0, 113, 10}
	for _, known := range []bool{true, false} {
		backend := &protocolLeases{err: ErrUnknownLease}
		if known {
			backend.err = ErrUnavailable
		}
		server := &Server{cfg: cfg, leases: backend}
		reply, destination := protocolReply(t, server, packet)
		if known && (reply == nil || reply.MessageType() != dhcpv4.MessageTypeNak || !destination.IP.Equal(net.IPv4bcast)) {
			t.Fatal("known invalid direct renewal did not get broadcast NAK")
		}
		if !known && reply != nil {
			t.Fatal("unknown invalid direct renewal answered")
		}
	}
}

func TestProtocolRelayedInformAndReleaseDeclineSilence(t *testing.T) {
	cfg := relayConfig()
	peer := &net.UDPAddr{IP: net.IP(cfg.TrustedRelays[0].SourceIP.AsSlice()), Port: serverPort}
	for _, message := range []dhcpv4.MessageType{dhcpv4.MessageTypeInform, dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline} {
		backend := &protocolLeases{}
		server := &Server{cfg: cfg, leases: backend}
		packet := clientPacket(message, dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, []byte{1, 0}))
		packet.GatewayIPAddr, packet.HopCount = net.IP(cfg.TrustedRelays[0].GatewayIP.AsSlice()), 2
		if message == dhcpv4.MessageTypeInform || message == dhcpv4.MessageTypeRelease {
			packet.ClientIPAddr = net.IP{192, 0, 2, 10}
		}
		if message != dhcpv4.MessageTypeInform {
			packet.UpdateOption(dhcpv4.OptServerIdentifier(net.IP(cfg.ServerIP.AsSlice())))
		}
		if message == dhcpv4.MessageTypeDecline {
			packet.UpdateOption(dhcpv4.OptRequestedIPAddress(net.IP{192, 0, 2, 10}))
		}
		wire, dest, err := server.handlePacket(context.Background(), packet.ToBytes(), peer)
		if err != nil {
			t.Fatal(err)
		}
		if message != dhcpv4.MessageTypeInform {
			if wire != nil || backend.calls != 1 {
				t.Fatal("relay release/decline replied or not applied")
			}
			continue
		}
		if wire == nil || backend.calls != 0 || dest.Port != serverPort {
			t.Fatal("relay INFORM behavior incorrect")
		}
		reply, err := dhcpv4.FromBytes(wire)
		if err != nil || reply.MessageType() != dhcpv4.MessageTypeAck || !reply.YourIPAddr.IsUnspecified() || reply.Options.Has(dhcpv4.OptionIPAddressLeaseTime) || !bytes.Equal(reply.Options.Get(dhcpv4.OptionRelayAgentInformation), []byte{1, 0}) {
			t.Fatal("relay INFORM options incorrect", err)
		}
	}
}
