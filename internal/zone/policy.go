package zone

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// DefaultNotifyPort is the port of a notifyTargets entry without one.
const DefaultNotifyPort = 53

// KeyName returns the canonical form of a TSIG key name: lower-case and
// absolute (trailing dot). TSIG key names are domain names (RFC 8945 §4.2)
// and compare case-insensitively; every key lookup goes through this form.
func KeyName(name string) string {
	return dns.CanonicalName(strings.TrimSpace(name))
}

// compileKeyRefs validates spec.policy.transferTSIGKeyRefs and returns the
// canonical key names, without duplicates.
func compileKeyRefs(refs []string, path *field.Path) ([]string, field.ErrorList) {
	var errs field.ErrorList
	var out []string
	for i, r := range refs {
		n := KeyName(r)
		if _, ok := dns.IsDomainName(n); !ok || n == "." {
			errs = append(errs, field.Invalid(path.Index(i), r, "must be a TSIG key name (a domain name such as transfer-key.)"))
			continue
		}
		dup := false
		for _, have := range out {
			dup = dup || have == n
		}
		if !dup {
			out = append(out, n)
		}
	}
	return out, errs
}

// notifyModes lists the valid NotifyMode values for error messages.
var notifyModes = []v1alpha1.NotifyMode{v1alpha1.NotifyNone, v1alpha1.NotifyZoneNameServers, v1alpha1.NotifySpecified, v1alpha1.NotifyBoth}

// compileNotify validates spec.policy.notify and notifyTargets. An empty
// or invalid mode is none; invalid targets are dropped.
func compileNotify(p v1alpha1.ZonePolicy, path *field.Path) (v1alpha1.NotifyMode, []netip.AddrPort, field.ErrorList) {
	mode := p.Notify
	if mode == "" {
		mode = v1alpha1.NotifyNone
	}
	var errs field.ErrorList
	switch mode {
	case v1alpha1.NotifyNone, v1alpha1.NotifyZoneNameServers, v1alpha1.NotifySpecified, v1alpha1.NotifyBoth:
	default:
		errs = append(errs, field.NotSupported(path.Child("notify"), p.Notify, notifyModes))
		mode = v1alpha1.NotifyNone
	}
	var targets []netip.AddrPort
	for i, t := range p.NotifyTargets {
		ap, err := ParseNotifyTarget(t)
		if err != nil {
			errs = append(errs, field.Invalid(path.Child("notifyTargets").Index(i), t, err.Error()))
			continue
		}
		targets = append(targets, ap)
	}
	return mode, targets, errs
}

// ParseNotifyTarget parses one notifyTargets entry: an IP address, or an
// address and port ("192.0.2.1:5353", "[2001:db8::1]:5353"). The port
// defaults to DefaultNotifyPort.
func ParseNotifyTarget(s string) (netip.AddrPort, error) {
	t := strings.TrimSpace(s)
	if a, err := netip.ParseAddr(t); err == nil && a.Zone() == "" {
		return netip.AddrPortFrom(a.Unmap(), DefaultNotifyPort), nil
	}
	ap, err := netip.ParseAddrPort(t)
	if err != nil || ap.Addr().Zone() != "" || ap.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("must be an IP address with an optional port, e.g. 192.0.2.1 or [2001:db8::1]:5353")
	}
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()), nil
}

// claim records the identity of an endpoint that is not served (disabled,
// expired or view-scoped) so a later object cannot take it over. Endpoints
// whose owner or type does not parse claim nothing.
func (b *Builder) claim(source string, ep *v1alpha1.Endpoint) {
	owner, err := OwnerName(b.z.name, ep.DNSName)
	if err != nil {
		return
	}
	t, err := parseType(ep.RecordType)
	if err != nil {
		return
	}
	key, err := canonicalKey(owner)
	if err != nil {
		return
	}
	id := identity{key, t, ep.SetIdentifier}
	if _, ok := b.owners[id]; !ok {
		b.owners[id] = source
	}
}

// conflict builds the error for a CNAME exclusivity violation (RFC 1034
// §3.6.2) at key: Duplicated when the data already at the name came from
// another object (the later object loses, like any duplicate), Invalid
// when the object contradicts itself.
func (b *Builder) conflict(path *field.Path, value, key, source, why string) *field.Error {
	if prev := b.nameSource[key]; prev != "" && prev != source {
		return field.Invalid(path, value, fmt.Sprintf("%s: %s; the name is defined by %s", v1alpha1.ReasonDuplicated, why, prev))
	}
	return field.Invalid(path, value, fmt.Sprintf("%s: %s", v1alpha1.ReasonInvalid, why))
}

// occlusion enforces RFC 6672 §2.4: a DNAME owner has no data below it,
// because lookups below it are always redirected. The later endpoint
// loses: data added below an existing DNAME, or a DNAME added above
// existing data.
func (b *Builder) occlusion(path *field.Path, value, key string, rrtype uint16) *field.Error {
	if b.dnames > 0 {
		if d := b.dnameAbove(key); d != nil {
			return field.Invalid(path, value, fmt.Sprintf("%s: %s is below the DNAME at %s and would never be served (RFC 6672 §2.4)",
				v1alpha1.ReasonOccluded, nameFromKey(key), d.name))
		}
	}
	if rrtype != dns.TypeDNAME {
		return nil
	}
	var below *node
	b.z.tree.walkAfter(key, func(n *node) bool {
		if !isKeyPrefix(n.key, key) {
			return false
		}
		if !n.empty() {
			below = n
			return false
		}
		return true
	})
	if below != nil {
		return field.Invalid(path, value, fmt.Sprintf("%s: a DNAME at %s would hide the records at %s (RFC 6672 §2.4)",
			v1alpha1.ReasonOccluded, nameFromKey(key), below.name))
	}
	return nil
}

// dnameAbove returns the closest strict ancestor of key, up to and
// including the apex, that owns a DNAME, or nil.
func (b *Builder) dnameAbove(key string) *node {
	for i := len(key) - 1; i >= len(b.z.key); i-- {
		if i > 0 && key[i-1] != keySep {
			continue
		}
		if n := b.z.tree.search(key[:i]); n != nil && n.get(dns.TypeDNAME) != nil {
			return n
		}
	}
	return nil
}

// walkAfter calls fn for every node whose key sorts after k, in canonical
// order, until fn returns false. The descendants of k come first, so a
// subtree scan stops at the first key that is not below k.
func (t *tree) walkAfter(k string, fn func(*node) bool) {
	walkAfter(t.root, k, fn)
}

func walkAfter(n *node, k string, fn func(*node) bool) bool {
	for n != nil {
		if n.key > k {
			if !walkAfter(n.left, k, fn) || !fn(n) {
				return false
			}
		}
		n = n.right
	}
	return true
}

// ErrorReason classifies a build error from Builder.Add or Build into the
// condition reason a reconciler reports for it: v1alpha1.ReasonDuplicated
// (the identity or the name is held by an earlier object),
// v1alpha1.ReasonOccluded (below a DNAME), v1alpha1.ReasonOutsideZone, or
// v1alpha1.ReasonInvalid for everything else. Identity duplicates (the
// same owner, type and set identifier as an earlier object) are the only
// errors of Type field.ErrorTypeDuplicate; a CNAME conflict with another
// object is ReasonDuplicated with Type field.ErrorTypeInvalid.
func ErrorReason(e *field.Error) string {
	switch {
	case e == nil:
		return ""
	case strings.HasPrefix(e.Detail, v1alpha1.ReasonDuplicated+": "):
		return v1alpha1.ReasonDuplicated
	case strings.HasPrefix(e.Detail, v1alpha1.ReasonOccluded+": "):
		return v1alpha1.ReasonOccluded
	case strings.HasSuffix(e.Field, ".dnsName") && strings.Contains(e.Detail, " is outside zone "):
		return v1alpha1.ReasonOutsideZone
	}
	return v1alpha1.ReasonInvalid
}
