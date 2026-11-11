package zone

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/miekg/dns"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// servedTypes are the zone types a complete serving image can carry. The
// DNSZone schema also admits external, catalog and secondaryCatalog, which no
// release serves yet.
var servedTypes = []v1alpha1.ZoneType{
	v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward, v1alpha1.ZoneTypeSecondary, v1alpha1.ZoneTypeStub,
}

// BuildNonprimary builds the two models a complete serving image keeps for a
// forward, secondary or stub zone configuration: the routing model of the
// desired configuration (a placeholder for secondary and stub zones, whose
// data is acquired at run time) and the closed coverage that answers while
// the zone is not admitted. sets are the record sets selected for the zone.
//
// A forward zone holds no records, so a record set that names one can never
// change its model: Build leaves those endpoints out and reports them per
// record set, and that is not an error here. Any error returned means the
// image cannot carry this configuration at all (ServingErrors reports the
// same condition as field errors before the object is stored).
func BuildNonprimary(config *v1alpha1.DNSZone, sets []*v1alpha1.DNSRecordSet, now time.Time) (model, coverage *Zone, err error) {
	if config == nil {
		return nil, nil, fmt.Errorf("zone configuration is required")
	}
	switch config.Spec.Type {
	case v1alpha1.ZoneTypePrimary:
		return nil, nil, fmt.Errorf("a primary zone is built from its committed data")
	case v1alpha1.ZoneTypeSecondary:
		model, err = BuildSecondaryPlaceholder(config)
	case v1alpha1.ZoneTypeStub:
		model, err = BuildStubPlaceholder(config)
	default:
		var errs Errors
		model, errs = Build(config, sets, BuildOptions{Now: now})
		if invalid := append(append(field.ErrorList{}, errs.Zone...), errs.Policy...); len(invalid) > 0 {
			err = invalid.ToAggregate()
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if model == nil {
		return nil, nil, fmt.Errorf("zone build returned no model")
	}
	closed := config.DeepCopy()
	closed.Spec.Type = v1alpha1.ZoneTypePrimary
	if coverage, err = BuildCommittedUnavailable(closed); err != nil {
		return nil, nil, err
	}
	return model, coverage, nil
}

// ServingErrors reports why a complete serving image cannot carry dz, as
// field errors of the object: a zone type no release serves, a zone name
// that is not an absolute DNS name, a policy (query, transfer or notify)
// that does not compile, or an update mode or update policy of a primary
// zone that the primary runtime does not compile (only deny and allow, and
// grants it can compile). It is empty exactly when the image accepts the
// configuration: the name rule of its decoder, then its model builders,
// BuildCommittedUnavailable for a primary zone and BuildNonprimary for the
// others.
//
// The SQLite driver publishes one complete image or nothing, so it refuses
// to store a zone with such an error instead of serving nothing at all
// (D373). The Kubernetes controller keeps reporting the same errors as
// conditions of the one zone.
func ServingErrors(dz *v1alpha1.DNSZone) field.ErrorList {
	if dz == nil {
		return field.ErrorList{field.Required(field.NewPath("spec"), "zone configuration is required")}
	}
	spec := field.NewPath("spec")
	served := false
	for _, t := range servedTypes {
		served = served || dz.Spec.Type == t
	}
	if !served {
		return field.ErrorList{field.NotSupported(spec.Child("type"), dz.Spec.Type, servedTypes)}
	}
	if _, err := dynamicupdate.CanonicalName(dz.Spec.ZoneName); err != nil {
		return field.ErrorList{field.Invalid(spec.Child("zoneName"), dz.Spec.ZoneName, "must be an absolute DNS name with a trailing dot")}
	}
	// Every zone type keeps the closed coverage of its configuration, which
	// is the configuration built as a primary zone.
	closed := dz.DeepCopy()
	closed.Spec.Type = v1alpha1.ZoneTypePrimary
	b, errs := NewBuilder(closed, BuildOptions{})
	if b != nil {
		errs = append(errs, b.PolicyErrors()...)
	}
	if dz.Spec.Type == v1alpha1.ZoneTypeForward {
		// The routing model of a forward zone has rules of its own.
		_, own := Build(dz, nil, BuildOptions{})
		errs = appendNew(errs, own.Zone)
		errs = appendNew(errs, own.Policy)
	}
	if dz.Spec.Type == v1alpha1.ZoneTypePrimary {
		errs = append(errs, updateModeErrors(dz)...)
	}
	if len(errs) > 0 {
		return errs
	}
	// The builders themselves have the last word, so this function and the
	// image can never disagree about a configuration.
	var err error
	if dz.Spec.Type == v1alpha1.ZoneTypePrimary {
		_, err = BuildCommittedUnavailable(dz)
	} else {
		_, _, err = BuildNonprimary(dz, nil, time.Time{})
	}
	if err != nil {
		return field.ErrorList{field.Invalid(spec, dz.Spec.Type, err.Error())}
	}
	return nil
}

// servedUpdateModes are the spec.policy.update values the primary runtime
// compiles. The schema admits every access mode for the field, as it did in
// 0.1.0, where updates were a placeholder; the committed policy of a primary
// zone with any other mode cannot be compiled, so the zone would never be
// committed or served (D373).
var servedUpdateModes = []v1alpha1.AccessMode{v1alpha1.AccessDeny, v1alpha1.AccessAllow}

func updateModeErrors(dz *v1alpha1.DNSZone) field.ErrorList {
	mode := dz.Spec.Policy.Update
	if mode != "" && mode != v1alpha1.AccessDeny && mode != v1alpha1.AccessAllow {
		return field.ErrorList{field.NotSupported(field.NewPath("spec", "policy", "update"), mode, servedUpdateModes)}
	}
	return updatePolicyErrors(dz)
}

// updatePolicyErrors refuses the spec.policy.updatePolicies entries the
// primary runtime cannot compile, whatever the update mode: it compiles
// every entry, also of a zone that denies updates, and a zone whose policy
// does not compile is never committed or served (D373). The rules are
// those of the runtime (dynamicupdate.PreflightGrantSpecs and
// CompileGrantSpec): a key name that is not a name, an owner outside the
// zone or with a "*" other than a leading "*.", a type an update cannot
// carry, and the count and length bounds. The workspace bound of a zone
// that allows updates is checked as if every key were in the keyring, so
// adding a TSIG key later cannot make a stored policy fail.
func updatePolicyErrors(dz *v1alpha1.DNSZone) field.ErrorList {
	entries := dz.Spec.Policy.UpdatePolicies
	if len(entries) == 0 {
		return nil
	}
	path := field.NewPath("spec", "policy", "updatePolicies")
	refuse := func(e *dynamicupdate.GrantSpecError) field.ErrorList {
		p := path
		if e.Index >= 0 {
			p = path.Index(e.Index).Child(e.Field)
		}
		return field.ErrorList{field.Invalid(p, e.Value, e.Detail)}
	}
	apex, err := dynamicupdate.CanonicalName(dz.Spec.ZoneName)
	if err != nil {
		return nil // reported as spec.zoneName
	}
	specs := make([]dynamicupdate.GrantSpec, len(entries))
	for i, g := range entries {
		specs[i] = dynamicupdate.GrantSpec{KeyRef: g.TSIGKeyRef, Name: g.Name, Types: g.Types}
	}
	if e := dynamicupdate.PreflightGrantSpecs(specs, dynamicupdate.MaxPolicyBytes); e != nil {
		return refuse(e)
	}
	credentials := map[string]dynamicupdate.Credential{}
	grants := map[string]dynamicupdate.Grant{}
	version := strings.Repeat("v", dynamicupdate.MaxIdentityBytes)
	for i, spec := range specs {
		grant, e := dynamicupdate.CompileGrantSpec(apex, i, spec, tsigKeyName)
		if e != nil {
			return refuse(e)
		}
		credentials[grant.Key] = dynamicupdate.Credential{Key: grant.Key, Version: version}
		encoded, _ := json.Marshal(grant)
		grants[string(encoded)] = grant
	}
	if dz.Spec.Policy.Update != v1alpha1.AccessAllow {
		// A zone that denies updates compiles no grant into its policy.
		return nil
	}
	var cs []dynamicupdate.Credential
	for _, c := range credentials {
		cs = append(cs, c)
	}
	var gs []dynamicupdate.Grant
	for _, g := range grants {
		gs = append(gs, g)
	}
	if _, err := dynamicupdate.PolicyAllocation(cs, gs, nil, dynamicupdate.MaxPolicyBytes); err != nil {
		return field.ErrorList{field.Invalid(path, len(entries), fmt.Sprintf("the policy needs more than its %d-byte workspace", dynamicupdate.MaxPolicyBytes))}
	}
	return nil
}

// tsigKeyName is the TSIG keyring's normalization of a key name
// (dnsserver.TSIGKeyName, which this package cannot import); a test of the
// primary runtime's policy compiler keeps the two equal.
func tsigKeyName(name string) string { return dns.CanonicalName(strings.TrimSpace(name)) }

// appendNew appends the errors of more that errs does not hold yet.
func appendNew(errs, more field.ErrorList) field.ErrorList {
	for _, e := range more {
		known := false
		for _, have := range errs {
			known = known || have.Error() == e.Error()
		}
		if !known {
			errs = append(errs, e)
		}
	}
	return errs
}
