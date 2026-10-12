package zoneview

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func TestServedAndCollector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := memory.New()
	defer func() { _ = st.Close() }()
	objs := []store.Object{
		&v1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "home"},
			Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary,
				SOA: &v1alpha1.SOASpec{MName: "ns1.home.example.", RName: "hostmaster.home.example."}, Nameservers: []string{"ns1.home.example."}},
		},
		&v1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "corp"},
			Spec:       v1alpha1.DNSZoneSpec{ZoneName: "corp.example.", Type: v1alpha1.ZoneTypeForward, Forward: &v1alpha1.ForwardSpec{UpstreamGroup: "default"}},
		},
		&v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nas", Namespace: "apps"},
			Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "home"},
				Endpoints: []v1alpha1.Endpoint{{DNSName: "nas.home.example", RecordType: "A", Targets: []string{"10.0.0.20", "10.0.0.21"}}}},
		},
	}
	for _, o := range objs {
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
	if Served(zc) != nil {
		t.Fatal("nothing is served before the cache syncs")
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(zc))
	go func() { _ = zc.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(Served(zc)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("never served; state=%v", zc.State())
		}
		time.Sleep(20 * time.Millisecond)
	}
	zs := Served(zc)
	if len(zs) != 1 || zs[0].Name() != "home.example." {
		t.Fatalf("forward zones must not be listed: %v", zs)
	}
	serial := zs[0].Serial()
	want := `
# HELP kudurru_zone_serial SOA serial this node serves, per authoritative zone.
# TYPE kudurru_zone_serial gauge
kudurru_zone_serial{zone="home.example."} ` + strconv.FormatUint(uint64(serial), 10) + `
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kudurru_zone_serial"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(NewCollector(zc), "kudurru_zone_records"); n != 2 {
		t.Fatalf("kudurru_zone_records series %d", n)
	}
	want = `
# HELP kudurru_zone_records Size of each authoritative zone served: resource records (kind=records) and owner names (kind=names).
# TYPE kudurru_zone_records gauge
kudurru_zone_records{kind="names",zone="home.example."} ` + strconv.Itoa(zs[0].Names()) + `
kudurru_zone_records{kind="records",zone="home.example."} ` + strconv.Itoa(zs[0].RecordCount()) + `
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "kudurru_zone_records"); err != nil {
		t.Fatal(err)
	}
	if zs[0].RecordCount() < 2 {
		t.Fatalf("records %d", zs[0].RecordCount())
	}
}
