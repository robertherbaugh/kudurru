package zoneview

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestBuilderAndSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	defer func() { _ = st.Close() }()
	zoneObj := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "home"},
		Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary,
			SOA: &v1alpha1.SOASpec{MName: "ns1.home.example.", RName: "hostmaster.home.example."}, Nameservers: []string{"ns1.home.example."}},
	}
	rrset := &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nas", Namespace: "apps"},
		Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"},
			Endpoints: []v1alpha1.Endpoint{{DNSName: "nas.home.example", RecordType: "A", Targets: []string{"10.0.0.20"}}}},
	}
	for _, o := range []store.Object{zoneObj, rrset} {
		if _, err := st.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	zc, err := zonecache.New(st, zonecache.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := zc.Register(Builder(nil)); err != nil {
		t.Fatal(err)
	}
	src := Source{Cache: zc}
	if src.Zones().Synced() {
		t.Fatal("must report unsynced before the cache starts")
	}
	go func() { _ = zc.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if set := src.Zones(); set.Synced() {
			z := set.Match("nas.home.example.")
			if z == nil || z.Name() != "home.example." {
				t.Fatalf("expected the home zone, got %v", z)
			}
			if z.RecordCount() == 0 {
				t.Fatal("zone has no records")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("zone view never synced; state=%v", zc.State())
}
