package main

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

const doctorNotifyTimeout = 3 * time.Second

// checkSelfNotify reports only the address in the authenticated listener reply.
// A locally selected socket address cannot establish post-NAT egress identity.
func checkSelfNotify(ctx context.Context, rep *doctorReport, cfg *config.Config, target, key string) {
	if target == "" {
		for _, listener := range cfg.Listeners.DNS {
			for _, protocol := range listener.Protocols {
				if protocol == "udp" {
					target = listener.Address
					break
				}
			}
			if target != "" {
				break
			}
		}
	}
	host, port, err := net.SplitHostPort(target)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 {
		rep.fail("notify", "select a reachable UDP listener with --notify-target host:port")
		return
	}
	switch host {
	case "::":
		host = "::1"
	case "", "0.0.0.0":
		host = "127.0.0.1"
	}
	target = net.JoinHostPort(host, port)
	selected := []config.TSIGKey{}
	for _, candidate := range cfg.TSIGKeys {
		if (key == "" && len(selected) == 0) || dnsserver.TSIGKeyName(candidate.Name) == dnsserver.TSIGKeyName(key) {
			selected = []config.TSIGKey{candidate}
		}
	}
	if len(selected) != 1 {
		rep.fail("notify", "--notify-key must name a configured tsigKeys entry with a readable secretFile")
		return
	}
	keys, err := dnsserver.KeyringFromConfig(selected)
	if err != nil {
		rep.fail("notify", "cannot load the selected TSIG secretFile; check its path, permissions and base64 encoding")
		return
	}
	name := dnsserver.TSIGKeyName(selected[0].Name)
	_, algorithm, _ := keys.Lookup(name)
	client := &dns.Client{Net: "udp", Timeout: doctorNotifyTimeout, TsigProvider: dnsserver.TSIGProvider(keys)}
	if cfg.Notify.Source != "" {
		client.Dialer = &net.Dialer{LocalAddr: &net.UDPAddr{IP: net.ParseIP(cfg.Notify.Source)}}
	}
	ctx, cancel := context.WithTimeout(ctx, doctorNotifyTimeout)
	defer cancel()
	message := new(dns.Msg)
	message.SetNotify(ingress.HealthName)
	message.SetTsig(name, algorithm, 30, time.Now().Unix())
	response, _, err := client.ExchangeContext(ctx, message, target)
	if err != nil {
		// Do not print peer-controlled errors, signatures or secret-file contents.
		rep.fail("notify", "authenticated round trip to %s failed; check UDP routing/firewall, notify.source binding, matching TSIG keys and the current ready leader", target)
		return
	}
	address, err := doctorNotifyAddress(response)
	if err != nil {
		rep.fail("notify", "listener did not attest to an authenticated egress address; check readiness, writer authority and TSIG configuration")
		return
	}
	rep.ok("notify", "listener %s observed egress %s (authenticated UDP NOTIFY)", target, address)
	if address.IsLoopback() {
		rep.warn("notify route", "loopback confirms the local path only; use --notify-target with the externally reachable listener address to inspect hairpin/egress routing")
	}
}

func doctorNotifyAddress(response *dns.Msg) (netip.Addr, error) {
	if response == nil || len(response.Question) != 1 || !strings.EqualFold(response.Question[0].Name, ingress.HealthName) || response.Question[0].Qtype != dns.TypeSOA || response.Question[0].Qclass != dns.ClassINET || !response.Response || response.Opcode != dns.OpcodeNotify || response.Rcode != dns.RcodeSuccess || !response.Authoritative || response.Truncated || response.IsTsig() == nil || response.IsTsig().MAC == "" || len(response.Answer) != 1 {
		return netip.Addr{}, fmt.Errorf("invalid self-NOTIFY acknowledgment")
	}
	txt, ok := response.Answer[0].(*dns.TXT)
	if !ok || !strings.EqualFold(txt.Hdr.Name, ingress.HealthName) || txt.Hdr.Class != dns.ClassINET || len(txt.Txt) != 1 {
		return netip.Addr{}, fmt.Errorf("invalid self-NOTIFY address")
	}
	address, err := netip.ParseAddr(txt.Txt[0])
	if err != nil || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("invalid self-NOTIFY address")
	}
	return address.Unmap(), nil
}
