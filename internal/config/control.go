package config

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// Control configures global DNS admission, before any local or cached answer.
type Control struct {
	AllowedNetworks []string `koanf:"allowedNetworks" json:"allowedNetworks"`
	BlockedQTypes   []string `koanf:"blockedQTypes" json:"blockedQTypes"`
}

// ControlRules contains detached, canonical typed control configuration.
type ControlRules struct {
	AllowedNetworks []netip.Prefix
	BlockedQTypes   []uint16
}

// Rules validates all inputs before an immutable serving view can be adopted.
func (c Control) Rules() (ControlRules, error) {
	var rules ControlRules
	if len(c.AllowedNetworks) > 1024 || len(c.BlockedQTypes) > 256 {
		return rules, fmt.Errorf("control: at most 1024 allowed networks and 256 blocked query types are supported")
	}
	for i, text := range c.AllowedNetworks {
		p, err := netip.ParsePrefix(text)
		if err != nil || p.Addr().Is4In6() && p.Bits() < 96 {
			return ControlRules{}, fmt.Errorf("control.allowedNetworks[%d]: expected an IPv4 or IPv6 CIDR; mapped IPv4 prefixes require at least 96 bits", i)
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		rules.AllowedNetworks = append(rules.AllowedNetworks, p.Masked())
	}
	for i, text := range c.BlockedQTypes {
		name := strings.ToUpper(text)
		qtype, ok := dns.StringToType[name]
		if !ok && strings.HasPrefix(name, "TYPE") {
			number := strings.TrimPrefix(name, "TYPE")
			valid := number != ""
			for _, digit := range number {
				valid = valid && digit >= '0' && digit <= '9'
			}
			value, err := strconv.ParseUint(number, 10, 16)
			if valid && err == nil {
				qtype, ok = uint16(value), true
			}
		}
		if !ok || qtype == 0 {
			return ControlRules{}, fmt.Errorf("control.blockedQTypes[%d]: expected a DNS type mnemonic or TYPE1 through TYPE65535", i)
		}
		rules.BlockedQTypes = append(rules.BlockedQTypes, qtype)
	}
	return rules, nil
}
