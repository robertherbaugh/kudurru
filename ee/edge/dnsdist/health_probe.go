package dnsdist

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/nodehealth"
)

var errHealthProof = errors.New("dnsdist: backend node health proof failed")

type healthCapability uint8

const (
	healthLegacy healthCapability = iota
	healthTargetV1
)

// healthProbe uses the original backend address and PROXYv2 secret. Results
// and errors never contain credentials, raw packets or upstream diagnostics.
func (s *Supervisor) healthProbe(ctx context.Context, backend edge.Backend, name string) (*dns.Msg, *dns.Msg, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", backend.Address)
	if err != nil {
		return nil, nil, errHealthProof
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, nil, errHealthProof
		}
	}
	source, err := netip.ParseAddrPort(conn.LocalAddr().String())
	if err != nil {
		return nil, nil, errHealthProof
	}
	destination, err := netip.ParseAddrPort(conn.RemoteAddr().String())
	if err != nil {
		return nil, nil, errHealthProof
	}
	source = netip.AddrPortFrom(source.Addr().Unmap(), source.Port())
	destination = netip.AddrPortFrom(destination.Addr().Unmap(), destination.Port())
	family := proxyproto.FamilyINET
	if destination.Addr().Is6() {
		family = proxyproto.FamilyINET6
	}
	header := proxyproto.Header{Command: proxyproto.CommandProxy, Family: family, Transport: proxyproto.TransportDatagram, Source: source, Destination: destination}
	if secret := s.cfg.Options.ProxyProtocolSecret; secret != "" {
		header.RawTLVs = proxyproto.AppendTLV(nil, edge.ProxyTLVSecret, []byte(secret))
	}
	wire, err := header.Append(nil)
	if err != nil {
		return nil, nil, errHealthProof
	}
	question := new(dns.Msg)
	question.SetQuestion(name, dns.TypeTXT)
	packet, err := question.Pack()
	if err != nil {
		return nil, nil, errHealthProof
	}
	wire = append(wire, packet...)
	if _, err := conn.Write(wire); err != nil {
		return nil, nil, errHealthProof
	}
	data := make([]byte, 4097)
	n, err := conn.Read(data)
	if err != nil || n > 4096 {
		return nil, nil, errHealthProof
	}
	response := new(dns.Msg)
	if err := response.Unpack(data[:n]); err != nil || !matchingHealthReply(question, response) {
		return nil, nil, errHealthProof
	}
	return question, response, nil
}

func matchingHealthReply(query, response *dns.Msg) bool {
	return response.Response && response.Id == query.Id && !response.Truncated && response.Opcode == dns.OpcodeQuery && len(response.Question) == 1 && strings.EqualFold(response.Question[0].Name, query.Question[0].Name) && response.Question[0].Qtype == dns.TypeTXT && response.Question[0].Qclass == dns.ClassINET
}

func ownerHealthReply(response *dns.Msg, name, owner string) bool {
	if response == nil || !response.Authoritative || response.Rcode != dns.RcodeSuccess || len(response.Answer) != 1 || len(response.Ns) != 0 || len(response.Extra) != 0 {
		return false
	}
	txt, ok := response.Answer[0].(*dns.TXT)
	if !ok || txt.Hdr.Class != dns.ClassINET || txt.Hdr.Ttl != 0 || !strings.EqualFold(txt.Hdr.Name, name) || len(txt.Txt) != 1 {
		return false
	}
	prefix := owner + " "
	if owner == "" || !strings.HasPrefix(txt.Txt[0], prefix) {
		return false
	}
	epoch := strings.TrimPrefix(txt.Txt[0], prefix)
	if epoch == "" {
		return false
	}
	for _, digit := range epoch {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(epoch, 10, 64)
	return err == nil
}

// classifyHealth is intentionally re-run for every candidate address/update.
// Generic health alone can only qualify the original startup-validator fallback.
func (s *Supervisor) classifyHealth(ctx context.Context, backend edge.Backend) (healthCapability, error) {
	_, generic, err := s.healthProbe(ctx, backend, nodehealth.Generic)
	if err != nil || !ownerHealthReply(generic, nodehealth.Generic, backend.Name) {
		return healthLegacy, errHealthProof
	}
	target := nodehealth.Target(backend.Name)
	_, positive, err := s.healthProbe(ctx, backend, target)
	if err != nil {
		return healthLegacy, err
	}
	if !ownerHealthReply(positive, target, backend.Name) {
		// Only an explicit unsupported response permits the original validated
		// fallback. Wrong ownership, SERVFAIL and malformed positive proof fail
		// closed; they cannot be silently reclassified as an older backend.
		switch positive.Rcode {
		case dns.RcodeNameError, dns.RcodeRefused, dns.RcodeNotImplemented:
			return healthLegacy, nil
		case dns.RcodeSuccess:
			if len(positive.Answer) == 0 {
				return healthLegacy, nil
			}
		}
		return healthLegacy, errHealthProof
	}
	// A fresh random negative identity is distinct from the expected owner.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return healthLegacy, errHealthProof
	}
	wrongOwner := backend.Name + "-negative-" + hex.EncodeToString(nonce[:])
	_, negative, err := s.healthProbe(ctx, backend, nodehealth.Target(wrongOwner))
	if err != nil {
		return healthLegacy, err
	}
	if negative.Authoritative && negative.Rcode == dns.RcodeRefused && len(negative.Answer) == 0 && len(negative.Ns) == 0 && len(negative.Extra) == 0 {
		return healthTargetV1, nil
	}
	if ownerHealthReply(negative, nodehealth.Target(wrongOwner), backend.Name) {
		return healthLegacy, nil
	}
	switch negative.Rcode {
	case dns.RcodeNameError, dns.RcodeRefused, dns.RcodeNotImplemented:
		return healthLegacy, nil
	}
	return healthLegacy, errHealthProof
}
