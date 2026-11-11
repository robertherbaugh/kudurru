package validator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// v01Dir holds the CRDs and samples of the 0.1.0 release, verbatim
// (testdata/v0.1.0/README.md).
const v01Dir = "testdata/v0.1.0"

// v01Validator is the validator of the 0.1.0 release: it admits, and
// defaults, exactly what that release stored.
func v01Validator(t *testing.T) *Validator {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(v01Dir, "crds", "*.yaml"))
	if err != nil || len(paths) != 7 {
		t.Fatalf("0.1.0 CRDs: %d files, %v", len(paths), err)
	}
	var docs [][]byte
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // G304: test fixtures
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, b)
	}
	v, err := NewFromCRDs(docs...)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// v01Samples are the sample files of 0.1.0, by kind.
var v01Samples = map[store.Kind]string{
	"DNSZone":        "kudurru.io_v1alpha1_dnszone.yaml",
	"DNSRecordSet":   "kudurru.io_v1alpha1_dnsrecordset.yaml",
	"BlockList":      "kudurru.io_v1alpha1_blocklist.yaml",
	"ClientGroup":    "kudurru.io_v1alpha1_clientgroup.yaml",
	"Policy":         "kudurru.io_v1alpha1_policy.yaml",
	"UnblockRequest": "kudurru.io_v1alpha1_unblockrequest.yaml",
	"DHCPScope":      "kudurru.io_v1alpha1_dhcpscope.yaml",
}

func v01Sample(t *testing.T, kind store.Kind) store.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(v01Dir, "samples", v01Samples[kind])) //nolint:gosec // G304: test fixtures
	if err != nil {
		t.Fatal(err)
	}
	obj := newTyped(t, kind)
	if err := yaml.UnmarshalStrict(b, obj); err != nil {
		t.Fatalf("the 0.1.0 sample of %s no longer decodes into the Go type: %v", kind, err)
	}
	return obj
}

// Every sample of the 0.1.0 release is still a valid object of this release,
// and so is the object 0.1.0 stored for it: the sample with the defaults of
// the 0.1.0 schema filled in. A schema change that breaks this turns an
// upgrade into a refusal (store/sqlite admitUpgrade) for an ordinary
// installation.
func TestV01SamplesRemainValid(t *testing.T) {
	old, current := v01Validator(t), testValidator(t)
	for kind := range v01Samples {
		t.Run(string(kind), func(t *testing.T) {
			if err := current.Validate(t.Context(), v01Sample(t, kind), nil); err != nil {
				t.Fatalf("this release refuses the 0.1.0 sample: %v", err)
			}
			stored := v01Sample(t, kind)
			if err := old.Validate(t.Context(), stored, nil); err != nil {
				t.Fatalf("the 0.1.0 schema refuses its own sample: %v", err)
			}
			if err := current.ValidateStored(t.Context(), stored); err != nil {
				t.Fatalf("this release refuses the object 0.1.0 stored for its sample: %v", err)
			}
			// And as an update of itself, which is how the API replaces it.
			if err := current.Validate(t.Context(), stored.DeepCopyObject().(store.Object), stored); err != nil {
				t.Fatalf("this release refuses to rewrite the object 0.1.0 stored: %v", err)
			}
		})
	}
	// 0.1.0 defaulted BlockList.spec.refreshInterval to 24h and stored it.
	// This release has no schema default (an omitted interval follows
	// blocking.refreshInterval); the stored 24h stays a valid explicit value.
	t.Run("BlockList refreshInterval default of 0.1.0", func(t *testing.T) {
		stored := v01Sample(t, "BlockList").(*v1alpha1.BlockList)
		stored.Spec.RefreshInterval = metav1.Duration{} // the sample sets one; most lists do not
		if err := old.Validate(t.Context(), stored, nil); err != nil || stored.Spec.RefreshInterval.Duration != 24*time.Hour {
			t.Fatalf("0.1.0 stored refreshInterval %s, %v; want its default 24h", stored.Spec.RefreshInterval.Duration, err)
		}
		if err := current.ValidateStored(t.Context(), stored); err != nil {
			t.Fatal(err)
		}
		fresh := v01Sample(t, "BlockList").(*v1alpha1.BlockList)
		fresh.Spec.RefreshInterval = metav1.Duration{}
		if err := current.Validate(t.Context(), fresh, nil); err != nil || fresh.Spec.RefreshInterval.Duration != 0 {
			t.Fatalf("this release defaulted refreshInterval to %s, %v", fresh.Spec.RefreshInterval.Duration, err)
		}
	})
}

// The objects 0.1.0 accepted and this release refuses, each with the field
// its refusal names: zone types that 0.1.0 reserved and did not serve,
// stored without the configuration they now require, and policy rewrites,
// which were an unchecked placeholder. An upgrade names them and stops
// (D373). A shape missing here that this release also refuses would be an
// upgrade refusal nobody decided on.
func TestV01ObjectsThisReleaseRefuses(t *testing.T) {
	old, current := v01Validator(t), testValidator(t)
	zone := func(mutate func(*v1alpha1.DNSZone)) store.Object {
		z := &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{ZoneName: "legacy.example."}}
		z.Name = "legacy"
		mutate(z)
		return z
	}
	policy := func(rewrites ...v1alpha1.Rewrite) store.Object {
		p := v01Sample(t, "Policy").(*v1alpha1.Policy)
		p.Spec.Rewrites = rewrites
		return p
	}
	for name, tc := range map[string]struct {
		obj   store.Object
		field string
		say   string
	}{
		"secondary zone without its configuration": {zone(func(z *v1alpha1.DNSZone) { z.Spec.Type = v1alpha1.ZoneTypeSecondary }), "spec", "secondary zones require secondary configuration"},
		"stub zone without its configuration":      {zone(func(z *v1alpha1.DNSZone) { z.Spec.Type = v1alpha1.ZoneTypeStub }), "spec", "stub zones require nameserver configuration"},
		"secondary zone without primaries": {zone(func(z *v1alpha1.DNSZone) {
			z.Spec.Type, z.Spec.Secondary = v1alpha1.ZoneTypeSecondary, &v1alpha1.SecondarySpec{Primaries: []string{}}
		}), "spec.secondary.primaries", "at least 1 items"},
		"stub zone without nameservers": {zone(func(z *v1alpha1.DNSZone) {
			z.Spec.Type, z.Spec.Stub = v1alpha1.ZoneTypeStub, &v1alpha1.StubSpec{Nameservers: []string{}}
		}), "spec.stub.nameservers", "at least 1 items"},
		"rewrite with a name only":            {policy(v1alpha1.Rewrite{Name: "a.example."}), "spec.rewrites[0]", "provide either a CNAME or targets, not both"},
		"rewrite with a CNAME and targets":    {policy(v1alpha1.Rewrite{Name: "a.example.", CNAME: "b.example.", Targets: []string{"192.0.2.1"}}), "spec.rewrites[0]", "provide either a CNAME or targets, not both"},
		"rewrite with a type in another case": {policy(v1alpha1.Rewrite{Name: "a.example.", Type: "a", Targets: []string{"192.0.2.1"}}), "spec.rewrites[0].type", "Unsupported value"},
	} {
		t.Run(name, func(t *testing.T) {
			stored := tc.obj.DeepCopyObject().(store.Object)
			if err := old.Validate(t.Context(), stored, nil); err != nil {
				t.Fatalf("0.1.0 did not accept this object: %v", err)
			}
			err := current.ValidateStored(t.Context(), stored)
			invalid, ok := AsError(err)
			if !ok {
				t.Fatalf("this release accepts the object (ValidateStored = %v); it is no longer a refusal", err)
			}
			found := false
			for _, cause := range invalid.Causes {
				found = found || cause.Field == tc.field && strings.Contains(cause.Message, tc.say)
			}
			if !found {
				t.Fatalf("the refusal does not name %s with %q: %v", tc.field, tc.say, err)
			}
		})
	}
	// The types 0.1.0 reserved are valid again with their configuration: a
	// secondary or stub zone that 0.1.0 stored complete becomes a served
	// zone of this release.
	for name, obj := range map[string]store.Object{
		"secondary zone": zone(func(z *v1alpha1.DNSZone) {
			z.Spec.Type, z.Spec.Secondary = v1alpha1.ZoneTypeSecondary, &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}}
		}),
		"stub zone": zone(func(z *v1alpha1.DNSZone) {
			z.Spec.Type, z.Spec.Stub = v1alpha1.ZoneTypeStub, &v1alpha1.StubSpec{Nameservers: []string{"192.0.2.53"}}
		}),
	} {
		t.Run(name+" with its configuration", func(t *testing.T) {
			if err := old.Validate(t.Context(), obj, nil); err != nil {
				t.Fatal(err)
			}
			if err := current.ValidateStored(t.Context(), obj); err != nil {
				t.Fatalf("this release refuses a complete %s that 0.1.0 stored: %v", name, err)
			}
		})
	}
}
