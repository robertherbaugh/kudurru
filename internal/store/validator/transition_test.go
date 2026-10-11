package validator

import (
	"testing"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

func TestZoneNameIsImmutable(t *testing.T) {
	v := testValidator(t)
	old := stored(typedSample(t, "DNSZone"))

	t.Run("changed zoneName is rejected", func(t *testing.T) {
		upd := typedSample(t, "DNSZone")
		zoneOf(upd).Spec.ZoneName = "lab.example.com."
		causes := wantInvalid(t, v.Validate(ctx(), upd, old), "spec.zoneName", invalid)
		if c := causes[0]; c.Detail != "zoneName is immutable" {
			t.Fatalf("detail = %q, want the CRD rule message", c.Detail)
		}
	})
	t.Run("same zoneName with other changes is accepted", func(t *testing.T) {
		upd := typedSample(t, "DNSZone")
		zoneOf(upd).Spec.DefaultTTL = 600
		zoneOf(upd).Spec.Nameservers = append(zoneOf(upd).Spec.Nameservers, "ns3.home.example.com.")
		if err := v.Validate(ctx(), upd, old); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("identical object is accepted", func(t *testing.T) {
		if err := v.Validate(ctx(), typedSample(t, "DNSZone"), old); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("transition rules do not run on create", func(t *testing.T) {
		z := typedSample(t, "DNSZone")
		zoneOf(z).Spec.ZoneName = "lab.example.com."
		if err := v.Validate(ctx(), z, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("typed nil old means create", func(t *testing.T) {
		z := typedSample(t, "DNSZone")
		zoneOf(z).Spec.ZoneName = "lab.example.com."
		var none *v1alpha1.DNSZone
		if err := v.Validate(ctx(), z, none); err != nil {
			t.Fatal(err)
		}
	})
}

func TestZoneRefIsImmutable(t *testing.T) {
	v := testValidator(t)
	old := stored(typedSample(t, "DNSRecordSet"))
	cases := []struct {
		name   string
		mutate func(*v1alpha1.DNSRecordSet)
		reject bool
	}{
		{"other zone", func(r *v1alpha1.DNSRecordSet) { r.Spec.ZoneRef.Name = "lab-example-com" }, true},
		{"namespace added", func(r *v1alpha1.DNSRecordSet) { r.Spec.ZoneRef.Namespace = "dns" }, true},
		{"same zoneRef, new endpoint", func(r *v1alpha1.DNSRecordSet) {
			r.Spec.Endpoints = append(r.Spec.Endpoints, v1alpha1.Endpoint{DNSName: "printer.home.example.com", RecordType: "A", Targets: []string{"10.0.0.30"}})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upd := rrsetOf(typedSample(t, "DNSRecordSet"))
			tc.mutate(upd)
			err := v.Validate(ctx(), upd, old)
			if !tc.reject {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			causes := wantInvalid(t, err, "spec.zoneRef", invalid)
			if causes[0].Detail != "zoneRef is immutable" {
				t.Fatalf("detail = %q", causes[0].Detail)
			}
		})
	}
}

// TestRatcheting pins the apiserver's CRD validation ratcheting (GA since
// Kubernetes 1.33): an update may keep a value that a newer schema rejects,
// as long as that value does not change.
func TestRatcheting(t *testing.T) {
	v := testValidator(t)
	legacy := stored(typedSample(t, "DNSZone"))
	zoneOf(legacy).Spec.Type = "master" // written before the enum existed

	t.Run("unchanged invalid field is tolerated", func(t *testing.T) {
		upd := typedSample(t, "DNSZone")
		zoneOf(upd).Spec.Type = "master"
		zoneOf(upd).Spec.DefaultTTL = 600
		if err := v.Validate(ctx(), upd, legacy); err != nil {
			t.Fatalf("ratcheting should accept the unchanged legacy value: %v", err)
		}
	})
	t.Run("changing to another invalid value is rejected", func(t *testing.T) {
		upd := typedSample(t, "DNSZone")
		zoneOf(upd).Spec.Type = "slave"
		wantInvalid(t, v.Validate(ctx(), upd, legacy), "spec.type", notSupported)
	})
	t.Run("the same value is rejected on create", func(t *testing.T) {
		z := typedSample(t, "DNSZone")
		zoneOf(z).Spec.Type = "master"
		wantInvalid(t, v.Validate(ctx(), z, nil), "spec.type", notSupported)
	})
}

func TestUpdateMetadata(t *testing.T) {
	v := testValidator(t)
	old := stored(typedSample(t, "Policy"))

	t.Run("server-owned identity is filled from the stored object", func(t *testing.T) {
		upd := typedSample(t, "Policy") // no uid, resourceVersion, generation
		if err := v.Validate(ctx(), upd, old); err != nil {
			t.Fatal(err)
		}
		if upd.GetUID() != "" || upd.GetResourceVersion() != "" {
			t.Fatal("Validate must not stamp identity; the driver does (Apply contract item 3)")
		}
	})
	t.Run("a different uid is rejected", func(t *testing.T) {
		upd := typedSample(t, "Policy")
		upd.SetUID("someone-else")
		wantInvalid(t, v.Validate(ctx(), upd, old), "metadata.uid", invalid)
	})
	t.Run("old of another kind is an error", func(t *testing.T) {
		err := v.Validate(ctx(), typedSample(t, "Policy"), stored(typedSample(t, "BlockList")))
		if err == nil || IsInvalid(err) {
			t.Fatalf("expected a programming error, got %v", err)
		}
	})
}

func TestUnknownKind(t *testing.T) {
	v := testValidator(t)
	type notAKind struct{ v1alpha1.DNSZone }
	err := v.Validate(ctx(), &notAKind{}, nil)
	if !isUnknownKind(err) {
		t.Fatalf("want ErrUnknownKind, got %v", err)
	}
	if err := v.Validate(ctx(), nil, nil); err == nil {
		t.Fatal("nil object accepted")
	}
	var _ store.Object = &notAKind{}
}
