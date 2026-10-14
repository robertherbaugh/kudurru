package currentguard

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Policy workspace has an independent finite ceiling even with larger RR data.
const maxPolicyBytes int64 = 4 << 20

func (o *Owner) policyBudget() int64 { return min(maxPolicyBytes, o.options.Limits.MaxAggregateBytes) }

func (o *Owner) compile(ctx context.Context, keys *dnsserver.StaticKeyring, z *public.DNSZone) (*zonestate.Policy, error) {
	if keys == nil || z == nil || z.Spec.Type != public.ZoneTypePrimary || z.Name == "" || z.UID == "" || z.ResourceVersion == "" {
		return nil, pr.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	apex, err := dynamicupdate.CanonicalName(z.Spec.ZoneName)
	if err != nil {
		return nil, pr.ErrInvalid
	}
	if z.Spec.Policy.Update != "" && z.Spec.Policy.Update != public.AccessDeny && z.Spec.Policy.Update != public.AccessAllow {
		return nil, pr.ErrUnavailable
	}
	if err := o.preflight(z.Spec.Policy.UpdatePolicies); err != nil {
		return nil, err
	}
	policy := &zonestate.Policy{Enabled: z.Spec.Policy.Update == public.AccessAllow, AuthEpoch: 1}
	credentials := map[string]dynamicupdate.Credential{}
	grants := map[string]dynamicupdate.Grant{}
	for _, raw := range z.Spec.Policy.UpdatePolicies {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := dynamicupdate.CanonicalName(dnsserver.TSIGKeyName(raw.TSIGKeyRef))
		if err != nil || key == "." || raw.TSIGKeyRef == "" {
			return nil, pr.ErrInvalid
		}
		owner := raw.Name
		wild := strings.HasPrefix(owner, "*.")
		if wild {
			owner = strings.TrimPrefix(owner, "*.")
		}
		owner, err = dynamicupdate.CanonicalName(owner)
		if err != nil || strings.Contains(owner, "*") || !dns.IsSubDomain(apex, owner) {
			return nil, pr.ErrInvalid
		}
		if wild {
			owner = "*." + owner
		}
		var types []uint16
		anyType := false
		for _, name := range raw.Types {
			typ, err := grantType(name)
			if err != nil {
				return nil, err
			}
			if typ == dns.TypeANY {
				anyType = true
			} else {
				types = append(types, typ)
			}
		}
		if anyType {
			types = nil
		} else {
			sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
			n := 0
			for _, typ := range types {
				if n == 0 || types[n-1] != typ {
					types[n] = typ
					n++
				}
			}
			types = types[:n]
		}
		// Validate even disabled/missing-key grants before withholding them.
		identity, found := keys.Identity(key)
		if !policy.Enabled || !found {
			continue
		}
		credentials[key] = dynamicupdate.Credential{Key: key, Version: identity.CredentialVersion}
		grant := dynamicupdate.Grant{Key: key, Owner: owner, Types: types}
		encoded, _ := json.Marshal(grant)
		grants[string(encoded)] = grant
	}
	for _, c := range credentials {
		policy.Credentials = append(policy.Credentials, c)
	}
	for _, g := range grants {
		policy.Grants = append(policy.Grants, g)
	}
	sort.Slice(policy.Credentials, func(i, j int) bool { return policy.Credentials[i].Key < policy.Credentials[j].Key })
	// Match zonestate's deterministic Key/Owner/JSON-type-list ordering.
	sort.Slice(policy.Grants, func(i, j int) bool {
		a, b := policy.Grants[i], policy.Grants[j]
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		x, _ := json.Marshal(a.Types)
		y, _ := json.Marshal(b.Types)
		return string(x) < string(y)
	})
	if _, err := dynamicupdate.PolicyAllocation(policy.Credentials, policy.Grants, nil, o.policyBudget()); err != nil {
		return nil, pr.ErrCapacity
	}
	return policy, nil
}

func (o *Owner) preflight(grants []public.UpdatePolicy) error {
	if len(grants) > dynamicupdate.MaxPolicyEntries {
		return pr.ErrCapacity
	}
	left, typesLeft := o.policyBudget(), dynamicupdate.MaxTotalGrantTypes
	charge := func(n int64) bool {
		if n > left {
			return false
		}
		left -= n
		return true
	}
	for _, g := range grants {
		if len(g.TSIGKeyRef) > dynamicupdate.MaxNameBytes || len(g.Name) > dynamicupdate.MaxNameBytes+2 || len(g.Types) > typesLeft {
			return pr.ErrCapacity
		}
		typesLeft -= len(g.Types)
		if !charge(256 + int64(len(g.TSIGKeyRef))*4 + int64(len(g.Name))*4 + int64(len(g.Types))*32) {
			return pr.ErrCapacity
		}
		for _, typ := range g.Types {
			if len(typ) > 16 || !charge(int64(len(typ))) {
				return pr.ErrCapacity
			}
		}
	}
	return nil
}

func grantType(name string) (uint16, error) {
	name = strings.ToUpper(name)
	typ, ok := dns.StringToType[name]
	if !ok && strings.HasPrefix(name, "TYPE") {
		n, err := strconv.ParseUint(strings.TrimPrefix(name, "TYPE"), 10, 16)
		if err == nil {
			typ, ok = uint16(n), true
		}
	}
	if !ok || (typ != dns.TypeANY && (typ == 0 || typ == 65535 || typ == dns.TypeOPT || (typ >= dns.TypeTKEY && typ <= dns.TypeANY))) {
		return 0, pr.ErrInvalid
	}
	return typ, nil
}

func (o *Owner) checkPolicy(ctx context.Context, keys *dnsserver.StaticKeyring, z *public.DNSZone, observed zonestate.Policy) error {
	if observed.AuthEpoch == 0 {
		return pr.ErrUnavailable
	}
	if _, err := dynamicupdate.PolicyAllocation(observed.Credentials, observed.Grants, nil, o.policyBudget()); err != nil {
		return pr.ErrCapacity
	}
	expected, err := o.compile(ctx, keys, z)
	if err != nil {
		return err
	}
	observed.AuthEpoch = expected.AuthEpoch
	x, _ := json.Marshal(observed)
	y, _ := json.Marshal(expected)
	if string(x) != string(y) {
		return pr.ErrUnavailable
	}
	return nil
}
