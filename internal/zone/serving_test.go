package zone

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// servingConfig is a zone of the given type that a complete serving image
// carries, for the cases below to break.
func servingConfig(typ v1alpha1.ZoneType, mutate ...func(*v1alpha1.DNSZone)) *v1alpha1.DNSZone {
	return dnsZone("serving.example.", append([]func(*v1alpha1.DNSZone){func(dz *v1alpha1.DNSZone) {
		dz.UID = types.UID("serving-uid")
		dz.Spec.Type = typ
		switch typ {
		case v1alpha1.ZoneTypeSecondary:
			dz.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}}
		case v1alpha1.ZoneTypeStub:
			dz.Spec.Stub = &v1alpha1.StubSpec{Nameservers: []string{"192.0.2.53"}}
		case v1alpha1.ZoneTypeForward:
			dz.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "default"}
		}
	}}, mutate...)...)
}

// imageBuilds does what the complete serving image does with dz: the name
// rule of its decoder (internal/primaryruntime), then the model builders,
// BuildCommittedUnavailable for a primary zone (internal/primaryruntime) and
// BuildNonprimary for the others (internal/servingzones).
func imageBuilds(dz *v1alpha1.DNSZone) error {
	if _, err := dynamicupdate.CanonicalName(dz.Spec.ZoneName); err != nil {
		return err
	}
	if dz.Spec.Type == v1alpha1.ZoneTypePrimary {
		_, err := BuildCommittedUnavailable(dz)
		return err
	}
	_, _, err := BuildNonprimary(dz, nil, time.Time{})
	return err
}

// The store may admit a zone exactly when the image builders accept it:
// every zone ServingErrors passes builds, and every zone it refuses does not.
func TestServingErrorsMatchTheImageBuilders(t *testing.T) {
	served := []v1alpha1.ZoneType{v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward, v1alpha1.ZoneTypeSecondary, v1alpha1.ZoneTypeStub}
	type shape struct {
		name   string
		mutate func(*v1alpha1.DNSZone)
		field  string // the field the refusal names; "" when the shape is valid
	}
	shapes := []shape{
		{name: "valid"},
		{name: "valid ACLs and notify", mutate: func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy = v1alpha1.ZonePolicy{
				QueryAccess: v1alpha1.AccessUseNetworkACL, QueryACL: []string{"192.0.2.0/24", "!192.0.2.9"},
				ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: []string{"2001:db8::/32"},
				Notify: v1alpha1.NotifySpecified, NotifyTargets: []string{"192.0.2.2", "[2001:db8::2]:5353"},
			}
		}},
		{name: "query ACL entry that is not a network", field: "spec.policy.queryACL[0]", mutate: func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy.QueryAccess, dz.Spec.Policy.QueryACL = v1alpha1.AccessUseNetworkACL, []string{"10.0.0.0/33"}
		}},
		{name: "transfer ACL entry that is not a network", field: "spec.policy.transferACL[1]", mutate: func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy.ZoneTransfer, dz.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"192.0.2.1", "192.168.1"}
		}},
		{name: "notify target that is not an address", field: "spec.policy.notifyTargets[0]", mutate: func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy.Notify, dz.Spec.Policy.NotifyTargets = v1alpha1.NotifySpecified, []string{"ns2.example.net"}
		}},
		{name: "zone name without a trailing dot", field: "spec.zoneName", mutate: func(dz *v1alpha1.DNSZone) { dz.Spec.ZoneName = "serving.example" }},
		{name: "zone name with a label over 63 bytes", field: "spec.zoneName", mutate: func(dz *v1alpha1.DNSZone) {
			dz.Spec.ZoneName = strings.Repeat("a", 64) + ".example."
		}},
	}
	for _, typ := range served {
		for _, s := range shapes {
			t.Run(string(typ)+"/"+s.name, func(t *testing.T) {
				var mutate []func(*v1alpha1.DNSZone)
				if s.mutate != nil {
					mutate = append(mutate, s.mutate)
				}
				dz := servingConfig(typ, mutate...)
				before := dz.DeepCopy()
				errs := ServingErrors(dz)
				built := imageBuilds(dz)
				if (len(errs) == 0) != (built == nil) {
					t.Fatalf("ServingErrors %v, but the image builders say %v", errs, built)
				}
				if (s.field == "") != (len(errs) == 0) {
					t.Fatalf("ServingErrors = %v, want a refusal: %v", errs, s.field != "")
				}
				if s.field != "" && errs[0].Field != s.field {
					t.Fatalf("refusal names %q, want %q (%v)", errs[0].Field, s.field, errs)
				}
				if SecondaryIdentity(before) != SecondaryIdentity(dz) {
					t.Fatal("ServingErrors changed the object")
				}
			})
		}
	}
	// The types the DNSZone schema admits and no release serves.
	for _, typ := range []v1alpha1.ZoneType{v1alpha1.ZoneTypeExternal, v1alpha1.ZoneTypeCatalog, v1alpha1.ZoneTypeSecondaryCatalog, "future"} {
		t.Run(string(typ), func(t *testing.T) {
			dz := servingConfig(typ)
			errs := ServingErrors(dz)
			if len(errs) != 1 || errs[0].Field != "spec.type" || !strings.Contains(errs[0].Error(), `supported values: "primary", "forward", "secondary", "stub"`) {
				t.Fatalf("ServingErrors = %v", errs)
			}
			if imageBuilds(dz) == nil {
				t.Fatal("the image builders accept a type that is not served")
			}
		})
	}
	t.Run("fallback outside a primary zone", func(t *testing.T) {
		for _, typ := range served[1:] {
			dz := servingConfig(typ, func(dz *v1alpha1.DNSZone) { dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{} })
			errs, built := ServingErrors(dz), imageBuilds(dz)
			if (len(errs) == 0) != (built == nil) {
				t.Fatalf("%s: ServingErrors %v, but the image builders say %v", typ, errs, built)
			}
		}
		if errs := ServingErrors(servingConfig(v1alpha1.ZoneTypeForward, func(dz *v1alpha1.DNSZone) { dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{} })); len(errs) != 1 || errs[0].Field != "spec.fallback" {
			t.Fatalf("forward zone with fallback: %v", errs)
		}
	})
	t.Run("one refusal per error", func(t *testing.T) {
		// The coverage and the routing model of a forward zone compile the
		// same policy; each error is reported once.
		dz := servingConfig(v1alpha1.ZoneTypeForward, func(dz *v1alpha1.DNSZone) {
			dz.Spec.Policy.ZoneTransfer, dz.Spec.Policy.TransferACL = v1alpha1.AccessUseNetworkACL, []string{"nope", "192.0.2.300"}
		})
		errs := ServingErrors(dz)
		if len(errs) != 2 || errs[0].Field != "spec.policy.transferACL[0]" || errs[1].Field != "spec.policy.transferACL[1]" {
			t.Fatalf("ServingErrors = %v", errs)
		}
	})
	if errs := ServingErrors(nil); len(errs) != 1 {
		t.Fatalf("nil zone: %v", errs)
	}
}

// A record set that names a forward zone cannot change the zone: its
// endpoints are left out and reported for the record set, and the zone
// builds as it does without the record set.
func TestBuildNonprimaryIgnoresRecordSetsOfAForwardZone(t *testing.T) {
	dz := servingConfig(v1alpha1.ZoneTypeForward)
	rs := recordSet("default", "local", ep("www.serving.example", "A", "192.0.2.10"))
	rs.Spec.ZoneRef.Name = dz.Name
	served := ServedRecordSets(dz, []*v1alpha1.DNSRecordSet{rs})
	if len(served) != 1 {
		t.Fatalf("the record set is not selected for the zone: %d", len(served))
	}
	if _, errs := Build(dz, served, BuildOptions{Now: testNow}); len(errs.RecordSets["default/local"]) != 1 || len(errs.Zone) != 0 || len(errs.Policy) != 0 {
		t.Fatalf("Build does not report the record set: %+v", errs)
	}
	with, coverage, err := BuildNonprimary(dz, served, testNow)
	if err != nil {
		t.Fatalf("a record set of a forward zone failed the zone: %v", err)
	}
	without, _, err := BuildNonprimary(dz, nil, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if with.Type() != v1alpha1.ZoneTypeForward || with.RecordCount() != without.RecordCount() || with.ContentHash() != without.ContentHash() {
		t.Fatalf("the record set changed the forward zone: %d records, hash %s, want %d and %s", with.RecordCount(), with.ContentHash(), without.RecordCount(), without.ContentHash())
	}
	if coverage == nil || coverage.Name() != with.Name() || coverage.CommittedAvailableAt(testNow) {
		t.Fatal("no closed coverage for the forward zone")
	}
	// A zone configuration error still fails, with or without record sets.
	dz.Spec.Policy.QueryAccess, dz.Spec.Policy.QueryACL = v1alpha1.AccessUseNetworkACL, []string{"nope"}
	if _, _, err := BuildNonprimary(dz, served, testNow); err == nil || !strings.Contains(err.Error(), "spec.policy.queryACL[0]") {
		t.Fatalf("configuration error: %v", err)
	}
	if _, _, err := BuildNonprimary(servingConfig(v1alpha1.ZoneTypePrimary), nil, testNow); err == nil {
		t.Fatal("BuildNonprimary built a primary zone")
	}
	if _, _, err := BuildNonprimary(nil, nil, testNow); err == nil {
		t.Fatal("BuildNonprimary built a nil zone")
	}
}
