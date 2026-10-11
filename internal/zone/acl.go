package zone

import (
	"errors"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// errACLEntry explains the accepted syntax of an ACL line.
var errACLEntry = errors.New(`must be a CIDR or an IP address, optionally prefixed with "!" to deny`)

// ACL is an ordered address list in the DNSZone policy syntax: CIDRs or
// bare addresses, each optionally negated with a leading "!". The first
// entry that contains the client decides; a negated entry denies. No match
// denies. It is used for queryACL now and transferACL in M2.
type ACL []ACLEntry

// ACLEntry is one compiled ACL line.
type ACLEntry struct {
	Prefix netip.Prefix
	Deny   bool
}

// ParseACLEntry parses one ACL line.
func ParseACLEntry(s string) (ACLEntry, error) {
	t := strings.TrimSpace(s)
	var e ACLEntry
	if rest, ok := strings.CutPrefix(t, "!"); ok {
		e.Deny = true
		t = strings.TrimSpace(rest)
	}
	if strings.Contains(t, "/") {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			return e, errACLEntry
		}
		if p.Addr().Is4In6() {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		e.Prefix = p.Masked()
		return e, nil
	}
	a, err := netip.ParseAddr(t)
	if err != nil {
		return e, errACLEntry
	}
	a = a.Unmap().WithZone("")
	e.Prefix = netip.PrefixFrom(a, a.BitLen())
	return e, nil
}

// ParseACL compiles entries; errors carry the index below path.
func ParseACL(entries []string, path *field.Path) (ACL, field.ErrorList) {
	var errs field.ErrorList
	acl := make(ACL, 0, len(entries))
	for i, s := range entries {
		e, err := ParseACLEntry(s)
		if err != nil {
			errs = append(errs, field.Invalid(path.Index(i), s, err.Error()))
			continue
		}
		acl = append(acl, e)
	}
	return acl, errs
}

// Match returns the decision of the first entry containing addr and
// whether any entry matched.
func (a ACL) Match(addr netip.Addr) (allow, matched bool) {
	addr = addr.Unmap().WithZone("")
	for _, e := range a {
		if e.Prefix.Contains(addr) {
			return !e.Deny, true
		}
	}
	return false, false
}

// Allowed is Match with the default-deny rule applied.
func (a ACL) Allowed(addr netip.Addr) bool {
	allow, _ := a.Match(addr)
	return allow
}

// accessPolicy is a compiled queryAccess mode.
type accessPolicy struct {
	mode    v1alpha1.AccessMode
	acl     ACL
	nsAddrs []netip.Addr
}

func compileAccess(p v1alpha1.ZonePolicy, path *field.Path) (accessPolicy, field.ErrorList) {
	ap := accessPolicy{mode: p.QueryAccess}
	if ap.mode == "" {
		ap.mode = v1alpha1.AccessAllow
	}
	var errs field.ErrorList
	switch ap.mode {
	case v1alpha1.AccessAllow, v1alpha1.AccessDeny, v1alpha1.AccessAllowOnlyPrivate,
		v1alpha1.AccessAllowOnlyZoneNameServers, v1alpha1.AccessUseNetworkACL, v1alpha1.AccessZoneNameServersAndACL:
	default:
		errs = append(errs, field.NotSupported(path.Child("queryAccess"), p.QueryAccess, []v1alpha1.AccessMode{
			v1alpha1.AccessAllow, v1alpha1.AccessDeny, v1alpha1.AccessAllowOnlyPrivate,
			v1alpha1.AccessAllowOnlyZoneNameServers, v1alpha1.AccessUseNetworkACL, v1alpha1.AccessZoneNameServersAndACL,
		}))
	}
	acl, aerrs := ParseACL(p.QueryACL, path.Child("queryACL"))
	ap.acl = acl
	return ap, append(errs, aerrs...)
}

// allowed applies the mode to a client address. An invalid (zero) address
// is only allowed by the allow mode.
func (p *accessPolicy) allowed(client netip.Addr) bool {
	switch p.mode {
	case v1alpha1.AccessAllow:
		return true
	case v1alpha1.AccessDeny:
		return false
	}
	if !client.IsValid() {
		return false
	}
	client = client.Unmap().WithZone("")
	switch p.mode {
	case v1alpha1.AccessAllowOnlyPrivate:
		return IsPrivate(client)
	case v1alpha1.AccessAllowOnlyZoneNameServers:
		return p.isNameServer(client)
	case v1alpha1.AccessUseNetworkACL:
		return p.acl.Allowed(client)
	case v1alpha1.AccessZoneNameServersAndACL:
		return p.isNameServer(client) || p.acl.Allowed(client)
	}
	return false
}

func (p *accessPolicy) isNameServer(a netip.Addr) bool {
	for _, ns := range p.nsAddrs {
		if ns == a {
			return true
		}
	}
	return false
}

// IsPrivate reports whether a is a private, loopback or link-local address
// (RFC 1918, RFC 4193, 127/8, ::1, 169.254/16, fe80::/10), the
// allowOnlyPrivate access mode.
func IsPrivate(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()
}
