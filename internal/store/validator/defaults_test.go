package validator

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestDefaultsTypedDNSZone(t *testing.T) {
	v := testValidator(t)
	z := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "home"},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName: "home.example.",
			Type:     v1alpha1.ZoneTypePrimary,
			SOA:      &v1alpha1.SOASpec{MName: "ns1.home.example.", RName: "hostmaster.home.example."},
			Forward:  &v1alpha1.ForwardSpec{},
			DNSSEC:   &v1alpha1.DNSSECSpec{},
		},
	}
	if err := v.Validate(ctx(), z, nil); err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.DNSZoneSpec{
		ZoneName:   "home.example.",
		Type:       v1alpha1.ZoneTypePrimary,
		SOA:        &v1alpha1.SOASpec{MName: "ns1.home.example.", RName: "hostmaster.home.example.", Refresh: 3600, Retry: 900, Expire: 1209600, Minimum: 300},
		DefaultTTL: 300,
		Policy: v1alpha1.ZonePolicy{
			QueryAccess:  v1alpha1.AccessAllow,
			ZoneTransfer: v1alpha1.AccessDeny,
			Notify:       v1alpha1.NotifyNone,
			Update:       v1alpha1.AccessDeny,
		},
		// ForwardSpec.UpstreamGroup is omitempty, so an unset value is
		// absent on the wire and the schema default applies (D70).
		Forward: &v1alpha1.ForwardSpec{UpstreamGroup: "default"},
		DNSSEC:  &v1alpha1.DNSSECSpec{Algorithm: "ECDSAP256SHA256", Denial: "nsec"},
	}
	if !equality.Semantic.DeepEqual(z.Spec, want) {
		got, _ := json.Marshal(z.Spec)
		exp, _ := json.Marshal(want)
		t.Fatalf("defaults not applied:\n got %s\nwant %s", got, exp)
	}
	if z.APIVersion != "kudurru.io/v1alpha1" || z.Kind != "DNSZone" {
		t.Fatalf("type meta = %q %q", z.APIVersion, z.Kind)
	}
}

func TestDefaultsKeepExplicitValues(t *testing.T) {
	v := testValidator(t)
	z := typedSample(t, "DNSZone")
	zoneOf(z).Spec.SOA.Refresh = 7200
	zoneOf(z).Spec.DefaultTTL = 60
	if err := v.Validate(ctx(), z, nil); err != nil {
		t.Fatal(err)
	}
	if got := zoneOf(z).Spec; got.SOA.Refresh != 7200 || got.SOA.Retry != 900 || got.DefaultTTL != 60 || got.Policy.ZoneTransfer != v1alpha1.AccessUseNetworkACL {
		t.Fatalf("explicit values overwritten: %+v %+v", got.SOA, got.Policy)
	}
}

// TestDefaultsUnstructured covers the API/file path, where an absent field
// is distinguishable from a zero one, so struct-typed defaults apply too.
func TestDefaultsUnstructured(t *testing.T) {
	v := testValidator(t)
	cases := []struct {
		name  string
		doc   string
		check func(t *testing.T, u *unstructured.Unstructured)
	}{
		{"BlockList refreshInterval, mode and format", `
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: ads, namespace: dns}
spec:
  sources: [{url: "https://lists.example/hosts"}]
`, func(t *testing.T, u *unstructured.Unstructured) {
			bl := &v1alpha1.BlockList{}
			mustConvert(t, u, bl)
			if bl.Spec.RefreshInterval.Duration != 24*time.Hour || bl.Spec.Mode != "block" || bl.Spec.Sources[0].Format != "auto" {
				t.Fatalf("got %+v", bl.Spec)
			}
		}},
		{"Policy blockTTL and schedule tz", `
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: kids, namespace: dns}
spec:
  schedule: {start: "08:00", end: "20:00"}
`, func(t *testing.T, u *unstructured.Unstructured) {
			p := &v1alpha1.Policy{}
			mustConvert(t, u, p)
			if p.Spec.BlockTTL.Duration != 10*time.Minute || p.Spec.Schedule.TZ != "UTC" {
				t.Fatalf("got blockTTL=%v tz=%q", p.Spec.BlockTTL, p.Spec.Schedule.TZ)
			}
		}},
		{"DNSZone defaultTTL and SOA timers", `
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: {name: home}
spec:
  zoneName: home.example.
  type: forward
  soa: {}
  forward: {}
`, func(t *testing.T, u *unstructured.Unstructured) {
			z := &v1alpha1.DNSZone{}
			mustConvert(t, u, z)
			if s := z.Spec.SOA; s.Refresh != 3600 || s.Retry != 900 || s.Expire != 1209600 || s.Minimum != 300 || z.Spec.DefaultTTL != 300 {
				t.Fatalf("got %+v ttl=%d", s, z.Spec.DefaultTTL)
			}
			if z.Spec.Forward.UpstreamGroup != "default" {
				t.Fatalf("forward.upstreamGroup = %q", z.Spec.Forward.UpstreamGroup)
			}
			// spec.policy is absent and has no default of its own, so (as on
			// the apiserver) its members are not defaulted on this path.
			if _, found, _ := unstructured.NestedMap(u.Object, "spec", "policy"); found {
				t.Fatal("spec.policy was invented")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := unstructuredFromYAML(t, tc.doc)
			if err := v.ValidateUnstructured(ctx(), u, nil); err != nil {
				t.Fatal(err)
			}
			tc.check(t, u)
		})
	}
}

// TestTypedDurationZeroIsDefaulted pins the `omitzero` tag on the
// metav1.Duration fields: a zero value is absent on the wire, so the schema
// default applies to typed objects exactly as it does for a Kubernetes
// client (dev-docs/spikes/store-validator.md, D70).
func TestTypedDurationZeroIsDefaulted(t *testing.T) {
	v := testValidator(t)
	p := polOf(typedSample(t, "Policy"))
	p.Spec.BlockTTL = metav1.Duration{}
	if err := v.Validate(ctx(), p, nil); err != nil {
		t.Fatal(err)
	}
	if p.Spec.BlockTTL.Duration != 10*time.Minute {
		t.Fatalf("blockTTL = %v, want the schema default 10m", p.Spec.BlockTTL)
	}
}

// TestTypedAndUnstructuredAgree runs every sample through both entry points
// and requires the same defaulted object.
func TestTypedAndUnstructuredAgree(t *testing.T) {
	v := testValidator(t)
	for kind := range sampleFiles {
		t.Run(string(kind), func(t *testing.T) {
			typed := typedSample(t, kind)
			if err := v.Validate(ctx(), typed, nil); err != nil {
				t.Fatal(err)
			}
			u := unstructuredSample(t, kind)
			if err := v.ValidateUnstructured(ctx(), u, nil); err != nil {
				t.Fatal(err)
			}
			fromU := newTyped(t, kind)
			mustConvert(t, u, fromU)
			if !equality.Semantic.DeepEqual(typed, fromU) {
				t.Fatalf("typed and unstructured paths disagree:\n typed %+v\n unstr %+v", typed, fromU)
			}
		})
	}
}

func mustConvert(t *testing.T, u *unstructured.Unstructured, into store.Object) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, into); err != nil {
		t.Fatal(err)
	}
}
