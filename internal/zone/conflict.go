package zone

import (
	"fmt"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// ConflictingApices identifies duplicated valid apices across all zone
// types, even when their policy or data cannot be built. Index the result
// with dns.CanonicalName(dz.Spec.ZoneName). Conflict detection uses routing's
// label decoding and case folding; equivalent escaped presentations each
// get a result key. Nil objects and invalid apices are ignored. Callers
// supply distinct objects, as in a configuration snapshot. Inputs are not
// modified.
func ConflictingApices(zones []*v1alpha1.DNSZone) map[string]bool {
	owners := make(map[string][]string)
	for _, dz := range zones {
		if dz == nil {
			continue
		}
		apex, _, err := normalizedApex(dz.Spec.ZoneName)
		if err == nil {
			owners[apex] = append(owners[apex], dns.CanonicalName(dz.Spec.ZoneName))
		}
	}
	conflicts := make(map[string]bool)
	for _, names := range owners {
		if len(names) < 2 {
			continue
		}
		for _, name := range names {
			conflicts[name] = true
		}
	}
	return conflicts
}

// BuildUnavailable creates immutable authority coverage without serving
// data or selecting any configuration object's ACLs. Query access is allowed
// so the serving stage answers SERVFAIL rather than forwarding. It uses the
// unavailable-secondary representation, with no SOA, expiry or identity;
// runtime copies must never replace a zone with an empty SecondaryIdentity.
// Transfers are denied. The apex must be a valid nonempty domain name.
func BuildUnavailable(apex string) (*Zone, error) {
	name, key, err := normalizedApex(apex)
	if err != nil {
		return nil, err
	}
	z := &Zone{
		name: name, key: key, typ: v1alpha1.ZoneTypeSecondary,
		access:   accessPolicy{mode: v1alpha1.AccessAllow},
		transfer: accessPolicy{mode: v1alpha1.AccessDeny},
	}
	z.apex = z.tree.upsert(key, name)
	z.index = map[string]*node{key: z.apex}
	return z, nil
}

func normalizedApex(apex string) (name, key string, err error) {
	origin := fqdn(apex)
	key, err = canonicalKey(origin)
	if _, ok := dns.IsDomainName(origin); apex == "" || err != nil || !ok {
		return "", "", fmt.Errorf("zone apex must be a domain name")
	}
	return nameFromKey(key), key, nil
}
