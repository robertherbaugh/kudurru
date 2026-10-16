package dhcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// handlePacket ignores malformed/unsupported traffic before touching storage.
// An absent response is intentional for ignored packets, exhausted DISCOVERs,
// unknown INIT-REBOOT/renewal leases, RELEASE, and DECLINE.
func (s *Server) handlePacket(ctx context.Context, wire []byte, source *net.UDPAddr) ([]byte, *net.UDPAddr, error) {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	request := decodeClientPacket(wire, source)
	if request == nil || !s.acceptsPacket(request, source) {
		return nil, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	options := request.Options
	server := optionAddress(options.Get(dhcpv4.OptionServerIdentifier))
	if server.IsValid() && server != s.cfg.ServerIP {
		return nil, nil, nil
	}
	client := Client{MAC: request.ClientHWAddr.String(), Hostname: request.HostName()}
	if id := options.Get(dhcpv4.OptionClientIdentifier); id != nil {
		// Preserve every byte of the opaque option in a stable printable
		// identity. Empty ID lets the store use its distinct MAC namespace.
		client.ID = hex.EncodeToString(id)
	}
	requested := optionAddress(options.Get(dhcpv4.OptionRequestedIPAddress))
	current := packetAddress(request.ClientIPAddr)
	message := request.MessageType()
	if message == dhcpv4.MessageTypeRelease || message == dhcpv4.MessageTypeDecline {
		for _, forbidden := range []dhcpv4.OptionCode{dhcpv4.OptionIPAddressLeaseTime, dhcpv4.OptionParameterRequestList, dhcpv4.OptionMaximumDHCPMessageSize, dhcpv4.OptionClassIdentifier} {
			if options.Has(forbidden) {
				return nil, nil, nil
			}
		}
	}
	var lease Lease
	var err error
	responseType := dhcpv4.MessageTypeAck
	switch message {
	case dhcpv4.MessageTypeDiscover:
		if !current.IsUnspecified() || server.IsValid() {
			return nil, nil, nil
		}
		lease, err = s.leases.Offer(ctx, client, requested)
		if errors.Is(err, ErrNoAddress) || errors.Is(err, ErrUnavailable) {
			return nil, nil, nil
		}
		responseType = dhcpv4.MessageTypeOffer
	case dhcpv4.MessageTypeRequest:
		var mode RequestMode
		switch {
		case server.IsValid() && requested.IsValid() && current.IsUnspecified():
			mode = Selecting
		case !server.IsValid() && requested.IsValid() && current.IsUnspecified():
			mode = InitReboot
		case !server.IsValid() && !requested.IsValid() && !current.IsUnspecified():
			mode, requested = Renewing, current
		default:
			return nil, nil, nil
		}
		lease, err = s.leases.Acknowledge(ctx, client, requested, mode)
		// RFC 2131 section 4.3.2: a server without a record of an
		// INIT-REBOOT client MUST remain silent. A renewal without a
		// known binding likewise gives us no authority to ACK or NAK.
		if errors.Is(err, ErrUnknownLease) && mode != Selecting {
			return nil, nil, nil
		}
		if errors.Is(err, ErrUnknownLease) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoAddress) {
			// A remote unicast renewal cannot receive a giaddr=0 NAK:
			// RFC 2131 requires broadcasting that NAK on the client link.
			// Stay silent until the client rebinds through its relay.
			if s.cfg.RelayOnly && request.GatewayIPAddr.IsUnspecified() {
				return nil, nil, nil
			}
			responseType, err = dhcpv4.MessageTypeNak, nil
		}
	case dhcpv4.MessageTypeRelease:
		if !server.IsValid() || current.IsUnspecified() || requested.IsValid() {
			return nil, nil, nil
		}
		err = s.leases.Release(ctx, client, current)
		return nil, nil, mutationError(err)
	case dhcpv4.MessageTypeDecline:
		if !server.IsValid() || !current.IsUnspecified() || !requested.IsValid() {
			return nil, nil, nil
		}
		err = s.leases.Decline(ctx, client, requested)
		return nil, nil, mutationError(err)
	case dhcpv4.MessageTypeInform:
		if current.IsUnspecified() || server.IsValid() || requested.IsValid() || options.Has(dhcpv4.OptionIPAddressLeaseTime) || !s.cfg.usable(current) {
			return nil, nil, nil
		}
	default:
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if message != dhcpv4.MessageTypeInform && responseType != dhcpv4.MessageTypeNak &&
		(!lease.Address.Is4() || !s.cfg.Subnet.Contains(lease.Address)) {
		return nil, nil, errors.New("lease store returned an invalid address")
	}

	reply := s.reply(request, responseType, lease.Address)
	destination := &net.UDPAddr{IP: net.IPv4bcast, Port: clientPort}
	if responseType == dhcpv4.MessageTypeNak {
		reply.SetBroadcast()
	}
	if !request.GatewayIPAddr.IsUnspecified() {
		destination = &net.UDPAddr{IP: append(net.IP(nil), request.GatewayIPAddr...), Port: serverPort}
	} else if responseType != dhcpv4.MessageTypeNak && !current.IsUnspecified() {
		// RFC 2131 sections 4.1 and 4.3.5: configured clients receive
		// ACKs at ciaddr, including INFORM responses, without ARP tricks.
		destination.IP = net.IP(current.AsSlice())
	}
	// With ciaddr zero, broadcast is the RFC 2131 section 4.1 fallback
	// when a server cannot deliver directly to chaddr at the link layer.
	// Option 82 must be echoed whole or omitted (RFC 3046 section 2.2).
	if len(reply.ToBytes()) > replyLimit(request) {
		reply.DeleteOption(dhcpv4.OptionRelayAgentInformation)
	}
	return encodeReply(request, reply), destination, nil
}

func mutationError(err error) error {
	if errors.Is(err, ErrUnknownLease) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoAddress) {
		return nil
	}
	return err
}

func (s *Server) reply(request *dhcpv4.DHCPv4, message dhcpv4.MessageType, address netip.Addr) *dhcpv4.DHCPv4 {
	// Construct a response directly with the codec: a reply reuses the
	// request's transaction ID and never needs random-number generation.
	reply := &dhcpv4.DHCPv4{
		OpCode:        dhcpv4.OpcodeBootReply,
		HWType:        iana.HWTypeEthernet,
		TransactionID: request.TransactionID,
		ClientHWAddr:  append(net.HardwareAddr(nil), request.ClientHWAddr...),
		GatewayIPAddr: append(net.IP(nil), request.GatewayIPAddr...),
		Flags:         request.Flags & 0x8000,
		Options:       dhcpv4.OptionsFromList(dhcpv4.OptMessageType(message), dhcpv4.OptServerIdentifier(net.IP(s.cfg.ServerIP.AsSlice()))),
	}
	if id := request.Options.Get(dhcpv4.OptionClientIdentifier); id != nil {
		reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionClientIdentifier, id))
	}
	if relay := request.Options.Get(dhcpv4.OptionRelayAgentInformation); relay != nil {
		reply.UpdateOption(dhcpv4.OptGeneric(dhcpv4.OptionRelayAgentInformation, append([]byte(nil), relay...)))
	}
	if message == dhcpv4.MessageTypeNak {
		return reply
	}
	reply.ClientIPAddr = request.ClientIPAddr
	if request.MessageType() != dhcpv4.MessageTypeInform {
		reply.YourIPAddr = net.IP(address.AsSlice())
		// Offers hold an address briefly in storage but advertise the
		// lease duration the client will receive on ACK, not the hold.
		seconds := s.cfg.LeaseDuration / time.Second
		reply.UpdateOption(dhcpv4.OptIPAddressLeaseTime(seconds * time.Second))
		reply.UpdateOption(dhcpv4.OptRenewTimeValue((seconds / 2) * time.Second))
		reply.UpdateOption(dhcpv4.OptRebindingTimeValue((seconds * 7 / 8) * time.Second))
	}
	reply.UpdateOption(dhcpv4.OptSubnetMask(net.CIDRMask(s.cfg.Subnet.Bits(), 32)))
	limit := replyLimit(request)
	if len(reply.ToBytes()) > limit {
		reply.DeleteOption(dhcpv4.OptionRelayAgentInformation)
	}
	add := func(option dhcpv4.Option) {
		if !request.IsOptionRequested(option.Code) {
			return
		}
		reply.UpdateOption(option)
		if len(reply.ToBytes()) > limit {
			reply.DeleteOption(option.Code)
		}
	}
	networkOptions := make(map[byte]dhcpv4.Option, 3)
	if s.cfg.Router.IsValid() {
		option := dhcpv4.OptRouter(net.IP(s.cfg.Router.AsSlice()))
		networkOptions[option.Code.Code()] = option
	}
	if len(s.cfg.DNSServers) != 0 {
		addresses := make([]net.IP, len(s.cfg.DNSServers))
		for i, address := range s.cfg.DNSServers {
			addresses[i] = net.IP(address.AsSlice())
		}
		option := dhcpv4.OptDNS(addresses...)
		networkOptions[option.Code.Code()] = option
	}
	if s.cfg.DomainName != "" {
		option := dhcpv4.OptDomainName(s.cfg.DomainName)
		networkOptions[option.Code.Code()] = option
	}
	order := request.Options.Get(dhcpv4.OptionParameterRequestList)
	if order == nil {
		order = []byte{3, 6, 15}
	}
	for _, code := range order {
		if option, ok := networkOptions[code]; ok {
			add(option)
			delete(networkOptions, code)
		}
	}
	return reply
}

// The codec encodes all field and option values. Its default option sorting
// puts mask before router, but loses the client's parameter preference order.
// Reorder complete encoded options to honor RFC 2132 section 9.8, preserving
// the mandatory mask-before-router rule from section 3.3 and codec padding.
func encodeReply(request, reply *dhcpv4.DHCPv4) []byte {
	wire := reply.ToBytes()
	requested := request.Options.Get(dhcpv4.OptionParameterRequestList)

	var encoded [256][]byte
	var order []byte
	for offset := 240; offset < len(wire) && wire[offset] != 255; {
		code := wire[offset]
		if code == 0 {
			offset++
			continue
		}
		end := offset + 2 + int(wire[offset+1])
		if encoded[code] == nil {
			order = append(order, code)
		}
		encoded[code] = append(encoded[code], wire[offset:end]...)
		offset = end
	}
	out := append(make([]byte, 0, len(wire)), wire[:240]...)
	appendOption := func(code byte) {
		if code == dhcpv4.OptionRelayAgentInformation.Code() {
			return
		}
		out = append(out, encoded[code]...)
		encoded[code] = nil
	}
	if encoded[dhcpv4.OptionRouter.Code()] != nil {
		appendOption(dhcpv4.OptionSubnetMask.Code())
	}
	for _, code := range requested {
		appendOption(code)
	}
	for _, code := range order {
		appendOption(code)
	}
	out = append(out, encoded[dhcpv4.OptionRelayAgentInformation.Code()]...)
	out = append(out, 255)
	return append(out, make([]byte, max(0, 300-len(out)))...)
}

func decodeClientPacket(wire []byte, source *net.UDPAddr) *dhcpv4.DHCPv4 {
	if source == nil || (source.Port != clientPort && source.Port != serverPort) || source.IP.To4() == nil || len(wire) < 241 || len(wire) > maxPacketSize ||
		wire[0] != byte(dhcpv4.OpcodeBootRequest) || wire[1] != byte(iana.HWTypeEthernet) || wire[2] != 6 || wire[3] > 16 {
		return nil
	}
	request, err := dhcpv4.FromBytes(wire)
	if err != nil ||
		!request.YourIPAddr.IsUnspecified() || !request.ServerIPAddr.IsUnspecified() ||
		request.ClientHWAddr[0]&1 != 0 || bytes.Equal(request.ClientHWAddr, make([]byte, 6)) {
		return nil
	}
	if err := decodeOverloadedOptions(request, wire); err != nil || !validClientOptions(request.Options) {
		return nil
	}
	current := packetAddress(request.ClientIPAddr)
	gateway := packetAddress(request.GatewayIPAddr)
	if !gateway.IsUnspecified() {
		if !unicastAddress(gateway) || !unicastAddress(packetAddress(source.IP)) || source.Port != serverPort || request.HopCount == 0 || !validRelayOptions(request.Options) {
			return nil
		}
	} else if source.Port != clientPort || request.HopCount != 0 || request.Options.Has(dhcpv4.OptionRelayAgentInformation) {
		return nil
	}
	if !current.IsUnspecified() && (!unicastAddress(current) || gateway.IsUnspecified() && !source.IP.Equal(request.ClientIPAddr)) {
		return nil
	}
	return request
}

// The upstream codec decodes ordinary options. RFC 2132 option overload adds
// options from file, then sname, concatenating RFC 3396 fragments in that order.
func decodeOverloadedOptions(request *dhcpv4.DHCPv4, wire []byte) error {
	overload := request.Options.Get(dhcpv4.OptionOptionOverload)
	if overload == nil {
		return nil
	}
	if len(overload) != 1 || overload[0] < 1 || overload[0] > 3 {
		return errors.New("invalid option overload")
	}
	for _, field := range []struct {
		bit  byte
		data []byte
	}{{1, wire[108:236]}, {2, wire[44:108]}} {
		if overload[0]&field.bit == 0 {
			continue
		}
		// FromBytes requires an End marker, while Options.FromBytes does
		// not. Validate framing here before letting the codec decode it.
		if !terminatedOptions(field.data) {
			return errors.New("unterminated overloaded options")
		}
		options := make(dhcpv4.Options)
		if err := options.FromBytes(field.data); err != nil {
			return err
		}
		if options.Has(dhcpv4.OptionOptionOverload) || options.Has(dhcpv4.OptionDHCPMessageType) || options.Has(dhcpv4.OptionRelayAgentInformation) {
			return errors.New("control option in overloaded field")
		}
		for code, value := range options {
			request.Options[code] = append(request.Options[code], value...)
		}
	}
	return nil
}

func terminatedOptions(data []byte) bool {
	for offset := 0; offset < len(data); {
		code := data[offset]
		offset++
		switch code {
		case 255:
			return true
		case 0:
			continue
		}
		if offset >= len(data) || int(data[offset]) > len(data)-offset-1 {
			return false
		}
		offset += 1 + int(data[offset])
	}
	return false
}

func validClientOptions(options dhcpv4.Options) bool {
	message := options.Get(dhcpv4.OptionDHCPMessageType)
	if len(message) != 1 {
		return false
	}
	for _, option := range []dhcpv4.OptionCode{dhcpv4.OptionRequestedIPAddress, dhcpv4.OptionServerIdentifier} {
		if value, present := options[option.Code()]; present && (len(value) != 4 || !unicastAddress(optionAddress(value))) {
			return false
		}
	}
	if value, present := options[dhcpv4.OptionIPAddressLeaseTime.Code()]; present && len(value) != 4 {
		return false
	}
	if value, present := options[dhcpv4.OptionMaximumDHCPMessageSize.Code()]; present && (len(value) != 2 || binary.BigEndian.Uint16(value) < 576) {
		return false
	}
	if value, present := options[dhcpv4.OptionClientIdentifier.Code()]; present && (len(value) < 2 || len(value) > 255) {
		return false
	}
	if value, present := options[dhcpv4.OptionParameterRequestList.Code()]; present && (len(value) == 0 || len(value) > 255) {
		return false
	}
	if value, present := options[dhcpv4.OptionHostName.Code()]; present {
		if len(value) == 0 || !validHostname(string(value)) {
			return false
		}
		for _, character := range value {
			if character < 0x20 || character > 0x7e {
				return false
			}
		}
	}
	return true
}

func packetAddress(ip net.IP) netip.Addr {
	address, _ := netip.AddrFromSlice(ip.To4())
	return address
}

func optionAddress(value []byte) netip.Addr {
	if len(value) != 4 {
		return netip.Addr{}
	}
	return netip.AddrFrom4([4]byte(value))
}

func unicastAddress(address netip.Addr) bool {
	return usableIPv4(address)
}

func replyLimit(request *dhcpv4.DHCPv4) int {
	if maximum := request.Options.Get(dhcpv4.OptionMaximumDHCPMessageSize); maximum != nil {
		return min(int(binary.BigEndian.Uint16(maximum)), maxPacketSize)
	}
	return 576
}

// validRelayOptions validates option 82 TLVs without applying packet-supplied
// selection overrides. Link-selection, server-ID override and alternate relay
// ports require separate semantics, so this slice rejects those extensions.
func validRelayOptions(options dhcpv4.Options) bool {
	value, present := options[dhcpv4.OptionRelayAgentInformation.Code()]
	if !present {
		return true
	}
	if len(value) < 2 || len(value) > 255 {
		return false
	}
	for offset := 0; offset < len(value); {
		if offset+2 > len(value) {
			return false
		}
		code, length := value[offset], int(value[offset+1])
		if length > len(value)-offset-2 || code == 5 || code == 11 || code == 19 {
			return false
		}
		offset += 2 + length
	}
	return true
}

func (s *Server) acceptsPacket(request *dhcpv4.DHCPv4, source *net.UDPAddr) bool {
	gateway := packetAddress(request.GatewayIPAddr)
	if !gateway.IsUnspecified() {
		peer := packetAddress(source.IP)
		for _, relay := range s.cfg.TrustedRelays {
			if peer == relay.SourceIP && gateway == relay.GatewayIP {
				return true
			}
		}
		return false
	}
	current := packetAddress(request.ClientIPAddr)
	if s.cfg.RelayOnly && !s.cfg.usable(current) {
		return false
	}
	if !s.cfg.RelayOnly {
		return true
	}
	// Remote clients renew and release directly using the ordinary routed path.
	// Acknowledge(Renewing) requires an existing bound client identity in storage.
	return !current.IsUnspecified() && (request.MessageType() == dhcpv4.MessageTypeRequest || request.MessageType() == dhcpv4.MessageTypeRelease || request.MessageType() == dhcpv4.MessageTypeInform)
}
