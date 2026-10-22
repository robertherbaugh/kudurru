package plan

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Normalize canonicalizes RR content and provider options; labels/view/expiry
// are selection metadata, not remote record content. It never mutates input.
func Normalize(in public.Endpoint) (public.Endpoint, error) {
	o := *in.DeepCopy()
	o.DNSName = dns.CanonicalName(o.DNSName)
	o.RecordType = strings.ToUpper(o.RecordType)
	k := key(o)
	if err := k.Validate(); err != nil {
		return public.Endpoint{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if in.DNSName == "" || strings.TrimSpace(in.DNSName) != in.DNSName || len(o.Targets) == 0 || len(o.Targets) > kinds.MaxTargets || o.RecordTTL < 0 || o.RecordTTL > 2147483647 || len(o.ProviderSpecific) > 64 {
		return public.Endpoint{}, ErrInvalid
	}
	for i, t := range o.Targets {
		if t == "" || len(t) > 8192 || strings.ContainsFunc(t, unicode.IsControl) {
			return public.Endpoint{}, ErrInvalid
		}
		rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", o.DNSName, o.RecordTTL, o.RecordType, t))
		if err != nil {
			return public.Endpoint{}, fmt.Errorf("%w: invalid %s target", ErrInvalid, o.RecordType)
		}
		switch r := rr.(type) {
		case *dns.RRSIG:
			r.SignerName = dns.CanonicalName(r.SignerName)
		case *dns.NSEC:
			r.NextDomain = dns.CanonicalName(r.NextDomain)
		case *dns.RFC3597:
			r.Rdata = strings.ToLower(r.Rdata)
		case *dns.DNAME:
			r.Target = dns.CanonicalName(r.Target)
		case *dns.SVCB:
			r.Target = dns.CanonicalName(r.Target)
		case *dns.HTTPS:
			r.Target = dns.CanonicalName(r.Target)
		case *dns.NAPTR:
			r.Replacement = dns.CanonicalName(r.Replacement)
		case *dns.CNAME:
			r.Target = dns.CanonicalName(r.Target)
		case *dns.NS:
			r.Ns = dns.CanonicalName(r.Ns)
		case *dns.PTR:
			r.Ptr = dns.CanonicalName(r.Ptr)
		case *dns.MX:
			r.Mx = dns.CanonicalName(r.Mx)
		case *dns.SRV:
			r.Target = dns.CanonicalName(r.Target)
		case *dns.SOA:
			r.Ns = dns.CanonicalName(r.Ns)
			r.Mbox = dns.CanonicalName(r.Mbox)
		}
		wire := make([]byte, dns.Len(rr))
		if _, err := dns.PackRR(rr, wire, 0, nil, false); err != nil {
			return public.Endpoint{}, fmt.Errorf("%w: invalid RR wire encoding", ErrInvalid)
		}
		fields := strings.SplitN(rr.String(), "\t", 5)
		if len(fields) != 5 {
			return public.Endpoint{}, ErrInvalid
		}
		o.Targets[i] = fields[4]
	}
	slices.Sort(o.Targets)
	o.Targets = slices.Compact(o.Targets)
	if o.RecordType == "CNAME" && len(o.Targets) != 1 {
		return public.Endpoint{}, fmt.Errorf("%w: multiple CNAME targets", ErrInvalid)
	}
	slices.SortFunc(o.ProviderSpecific, func(a, b public.ProviderSpecific) int { return strings.Compare(a.Name, b.Name) })
	for i, p := range o.ProviderSpecific {
		if p.Name == "" || len(p.Name) > 256 || len(p.Value) > 8192 || strings.ContainsFunc(p.Name, unicode.IsControl) || strings.ContainsFunc(p.Value, unicode.IsControl) || i > 0 && p.Name == o.ProviderSpecific[i-1].Name {
			return public.Endpoint{}, fmt.Errorf("%w: provider option", ErrInvalid)
		}
	}
	o.Labels, o.Kudurru = nil, nil
	return o, nil
}

func key(e public.Endpoint) kinds.RecordKey {
	return kinds.RecordKey{Name: e.DNSName, Type: e.RecordType, SetIdentifier: e.SetIdentifier}
}
func keyString(k kinds.RecordKey) string { return k.Name + "\x00" + k.SetIdentifier + "\x00" + k.Type }
func merge(a, b public.Endpoint) (public.Endpoint, error) {
	if a.RecordTTL != b.RecordTTL || !slices.Equal(a.ProviderSpecific, b.ProviderSpecific) {
		return public.Endpoint{}, fmt.Errorf("%w: conflicting RRset TTL/options", ErrInvalid)
	}
	a.Targets = append(slices.Clone(a.Targets), b.Targets...)
	slices.Sort(a.Targets)
	a.Targets = slices.Compact(a.Targets)
	if len(a.Targets) > kinds.MaxTargets || a.RecordType == "CNAME" && len(a.Targets) != 1 {
		return public.Endpoint{}, ErrInvalid
	}
	return a, nil
}
