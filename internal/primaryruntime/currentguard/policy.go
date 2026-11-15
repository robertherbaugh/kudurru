package currentguard

import (
	"context"
	"encoding/json"
	"sort"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Policy workspace has an independent finite ceiling even with larger RR data.
const maxPolicyBytes = dynamicupdate.MaxPolicyBytes

func (o *Owner) policyBudget() int64 { return min(maxPolicyBytes, o.options.Limits.MaxAggregateBytes) }

func (o *Owner) compile(ctx context.Context, keys *dnsserver.StaticKeyring, z *public.DNSZone) (*zonestate.Policy, error) {
	return compilePolicy(ctx, keys, z, o.policyBudget())
}

// CompileUpdatePolicy derives a primary zone's current RFC 2136 credentials and
// grants from its update policy and the current verification ring. Drivers
// without a committed policy head (Kubernetes) use it on every UPDATE.
func CompileUpdatePolicy(ctx context.Context, keys *dnsserver.StaticKeyring, z *public.DNSZone, budget int64) (*zonestate.Policy, error) {
	if budget <= 0 || budget > maxPolicyBytes {
		budget = maxPolicyBytes
	}
	return compilePolicy(ctx, keys, z, budget)
}

func compilePolicy(ctx context.Context, keys *dnsserver.StaticKeyring, z *public.DNSZone, budget int64) (*zonestate.Policy, error) {
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
	specs := GrantSpecs(z.Spec.Policy.UpdatePolicies)
	if err := dynamicupdate.PreflightGrantSpecs(specs, budget); err != nil {
		return nil, pr.ErrCapacity
	}
	policy := &zonestate.Policy{Enabled: z.Spec.Policy.Update == public.AccessAllow, AuthEpoch: 1}
	credentials := map[string]dynamicupdate.Credential{}
	grants := map[string]dynamicupdate.Grant{}
	for i, spec := range specs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// zone.ServingErrors refuses to store a zone whose entries do not
		// compile here: both use dynamicupdate.CompileGrantSpec (D373).
		grant, bad := dynamicupdate.CompileGrantSpec(apex, i, spec, dnsserver.TSIGKeyName)
		if bad != nil {
			return nil, pr.ErrInvalid
		}
		key := grant.Key
		// Validate even disabled/missing-key grants before withholding them.
		identity, found := keys.Identity(key)
		if !policy.Enabled || !found {
			continue
		}
		credentials[key] = dynamicupdate.Credential{Key: key, Version: identity.CredentialVersion}
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
	if _, err := dynamicupdate.PolicyAllocation(policy.Credentials, policy.Grants, nil, budget); err != nil {
		return nil, pr.ErrCapacity
	}
	return policy, nil
}

// GrantSpecs is the configured update policy of a zone in the form
// dynamicupdate compiles.
func GrantSpecs(policies []public.UpdatePolicy) []dynamicupdate.GrantSpec {
	out := make([]dynamicupdate.GrantSpec, len(policies))
	for i, g := range policies {
		out[i] = dynamicupdate.GrantSpec{KeyRef: g.TSIGKeyRef, Name: g.Name, Types: g.Types}
	}
	return out
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
