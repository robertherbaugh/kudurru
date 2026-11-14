package zone

import (
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func TestConflictingApicesNormalizeAllTypes(t *testing.T) {
	object := func(name, apex string, typ v1alpha1.ZoneType) *v1alpha1.DNSZone {
		return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.DNSZoneSpec{ZoneName: apex, Type: typ}}
	}
	zones := []*v1alpha1.DNSZone{
		object("primary", "COPY.Example.", v1alpha1.ZoneTypePrimary),
		object("secondary", "copy.example", v1alpha1.ZoneTypeSecondary),
		object("forward", `c\079py.example.`, v1alpha1.ZoneTypeForward),
		object("stub", "COPY.example.", v1alpha1.ZoneTypeStub),
		object("external", "copy.example.", v1alpha1.ZoneTypeExternal),
		object("catalog", "copy.example.", v1alpha1.ZoneTypeCatalog),
		object("secondary-catalog", "copy.example.", v1alpha1.ZoneTypeSecondaryCatalog),
		object("root-one", ".", v1alpha1.ZoneTypePrimary),
		object("root-two", ".", v1alpha1.ZoneTypeForward),
		object("unique", "home.example.", v1alpha1.ZoneTypePrimary),
		object("empty", "", v1alpha1.ZoneTypePrimary),
		object("invalid-one", "bad..example.", v1alpha1.ZoneTypeSecondary),
		object("invalid-two", "bad..example.", v1alpha1.ZoneTypeSecondary),
		nil,
	}
	zones[0].Spec.Policy.QueryAccess = "invalid"
	before := make([]*v1alpha1.DNSZone, len(zones))
	for i, dz := range zones {
		if dz != nil {
			before[i] = dz.DeepCopy()
		}
	}
	want := map[string]bool{"copy.example.": true, `c\079py.example.`: true, ".": true}
	if got := ConflictingApices(zones); !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicts %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(zones, before) {
		t.Fatal("conflict detection mutated configuration")
	}
	slices.Reverse(zones)
	if got := ConflictingApices(zones); !reflect.DeepEqual(got, want) {
		t.Fatal("conflict detection depends on input ordering")
	}
}

func TestBuildUnavailableCoverage(t *testing.T) {
	for _, apex := range []string{"COPY.Example.", `c\079py.example`, "."} {
		t.Run(apex, func(t *testing.T) {
			z, err := BuildUnavailable(apex)
			if err != nil {
				t.Fatal(err)
			}
			if z.Type() != v1alpha1.ZoneTypeSecondary || z.SecondaryIdentity() != "" || z.SOA() != nil || z.RecordCount() != 0 || z.Names() != 1 {
				t.Fatal("unavailable coverage has data or a runtime identity")
			}
			if z.Name() != "copy.example." && z.Name() != "." {
				t.Fatalf("apex not normalized: %s", z.Name())
			}
			for _, now := range []time.Time{{}, time.Now(), time.Now().Add(time.Hour)} {
				if z.AvailableAt(now) {
					t.Fatal("unavailable coverage became available")
				}
			}
			client := netip.MustParseAddr("192.0.2.1")
			if !z.QueryAllowed(client) || z.TransferAllowed(client, "") || !z.Contains("www.copy.example.") {
				t.Fatal("coverage fails to terminate queries or permits transfers")
			}
			for range z.AXFR(0) {
				t.Fatal("unavailable coverage exported records")
			}
		})
	}
	for _, apex := range []string{"", "bad..example.", "\\999.example."} {
		if z, err := BuildUnavailable(apex); err == nil || z != nil {
			t.Fatalf("invalid apex %q accepted", apex)
		}
	}
}
