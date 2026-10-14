package zoneview

import (
	"context"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// The receiver only supplies immutable fixture records. The manager publishes
// them through its public snapshot API, allowing the tests to exercise the
// actual zoneview source without adding test mutation hooks to snapshots.
type zoneviewReceiver struct{ records []dns.RR }

func (r zoneviewReceiver) SOA(context.Context, *v1alpha1.DNSZone, string) (*dns.SOA, error) {
	return dns.Copy(r.records[0]).(*dns.SOA), nil
}

func (r zoneviewReceiver) AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error) {
	out := make([]dns.RR, len(r.records))
	for i, rr := range r.records {
		out[i] = dns.Copy(rr)
	}
	return out, nil
}

func TestSecondarySourceMatchesOnlyCurrentIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := memory.New()
	db, err := localdb.OpenSnapshotStore(ctx, filepath.Join(t.TempDir(), localdb.FileName))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		workers.Wait()
		_ = db.Close()
		_ = st.Close()
	})
	config := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "transferred"},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName: "transferred.example.", Type: v1alpha1.ZoneTypeSecondary,
			Secondary: &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}, TransferProtocol: "tcp"},
		},
		Status: v1alpha1.DNSZoneStatus{Serial: 12, ContentHash: "legacy", RecordCount: 99},
	}
	primary := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: "home.example.", Type: v1alpha1.ZoneTypePrimary}}
	root := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: ".", Type: v1alpha1.ZoneTypeForward}}
	for _, obj := range []store.Object{config, primary, root,
		&v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ignored", Namespace: "apps"},
			Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "transferred"}, Endpoints: []v1alpha1.Endpoint{{DNSName: "injected.transferred.example.", RecordType: "A", Targets: []string{"203.0.113.1"}}}},
		},
	} {
		if _, err := st.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Get(ctx, store.Key{Kind: "DNSZone", Name: "transferred"}, config); err != nil {
		t.Fatal(err)
	}
	cache, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Register(Builder(nil)); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var records []dns.RR
	for _, text := range []string{
		"transferred.example. 7200 IN SOA ns.transferred.example. admin.transferred.example. 4294967295 3600 60 7200 90",
		"transferred.example. 600 IN NS ns.transferred.example.",
		"www.transferred.example. 42 IN A 192.0.2.80",
	} {
		r, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, r)
	}
	manager, err := secondary.New(secondary.Options{Cache: cache, Blobs: db, ClusterID: "zoneview-test", Receiver: zoneviewReceiver{records}, Now: func() time.Time { return now }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workers.Go(func() { _ = cache.Start(ctx) })
	workers.Go(func() { _ = manager.Run(ctx) })
	id := zone.SecondaryIdentity(config)
	deadline := time.Now().Add(10 * time.Second)
	for manager.Snapshot().Lookup(id) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("no transfer published: %+v", manager.Statuses())
		}
		time.Sleep(time.Millisecond)
	}
	transfers := manager.Snapshot()
	transferred := transfers.Lookup(id)
	source := Source{Cache: cache, Secondary: manager}
	if got := source.Zones().Match("www.transferred.example."); got != transferred || got.Serial() != 4294967295 || got.SOA().Serial != 4294967295 {
		t.Fatal("source lost the external serial or did not select transferred data")
	}
	base, ok := zonecache.View[*zone.Snapshot](cache.Snapshot(), ViewName)
	if !ok {
		t.Fatal("zone builder did not publish")
	}
	placeholder := base.Match("injected.transferred.example.")
	if placeholder == nil || placeholder.Type() != v1alpha1.ZoneTypeSecondary || placeholder.AvailableAt(now) || placeholder.RecordCount() != 0 || placeholder.Serial() != 0 || placeholder.SecondaryIdentity() != id {
		t.Fatal("builder treated object records or legacy status as secondary data")
	}
	withoutManager := Source{Cache: cache}
	if got := withoutManager.Zones().Match("www.transferred.example."); got != placeholder || got.AvailableAt(now) {
		t.Fatal("source without runtime data dropped secondary coverage")
	}
	if nilCopies := (secondarySet{Set: base}); nilCopies.Match("www.transferred.example.") != placeholder {
		t.Fatal("nil transfer snapshot dropped placeholder")
	}
	served := Served(cache, manager)
	if len(served) != 2 || served[0] != transferred || served[1].Name() != "home.example." {
		t.Fatalf("served zones include a forwarder or omit transferred data: %v", served)
	}
	if onlyPrimary := Served(cache); len(onlyPrimary) != 1 || onlyPrimary[0].Name() != "home.example." {
		t.Fatal("served list included an unavailable placeholder")
	}
	for name, mutate := range map[string]func(*v1alpha1.DNSZone){
		"recreated":       func(z *v1alpha1.DNSZone) { z.UID = "new-instance" },
		"renamed":         func(z *v1alpha1.DNSZone) { z.Name = "other-object" },
		"origin":          func(z *v1alpha1.DNSZone) { z.Spec.ZoneName = "new.example." },
		"primary":         func(z *v1alpha1.DNSZone) { z.Spec.Secondary.Primaries = []string{"192.0.2.99"} },
		"key":             func(z *v1alpha1.DNSZone) { z.Spec.Secondary.TSIGKeyRef = "different-key" },
		"protocol":        func(z *v1alpha1.DNSZone) { z.Spec.Secondary.TransferProtocol = "tls" },
		"query-acl":       func(z *v1alpha1.DNSZone) { z.Spec.Policy.QueryACL = []string{"192.0.2.0/24"} },
		"transfer-policy": func(z *v1alpha1.DNSZone) { z.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow },
	} {
		t.Run(name, func(t *testing.T) {
			changed := config.DeepCopy()
			mutate(changed)
			newPlaceholder, err := zone.BuildSecondary(changed, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := zone.NewSnapshot(2, true, newPlaceholder, base.Match("home.example."), base.Match("outside.test."))
			if err != nil {
				t.Fatal(err)
			}
			current := secondarySet{Set: snapshot, copies: transfers}
			if got := current.Match("www." + changed.Spec.ZoneName); got != newPlaceholder || got.AvailableAt(now) || got.Serial() != 0 {
				t.Fatal("configuration change revived an old transfer")
			}
			if current.Match("home.example.") != base.Match("home.example.") {
				t.Fatal("identity mismatch changed unrelated primary")
			}
		})
	}
	// Status and generation updates retain configuration identity.
	metadataOnly := config.DeepCopy()
	metadataOnly.Generation++
	metadataOnly.Status.Serial = 1
	metadataOnly.ResourceVersion = "different"
	matching, err := zone.BuildSecondary(metadataOnly, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	matchingBase, err := zone.NewSnapshot(3, true, matching)
	if err != nil {
		t.Fatal(err)
	}
	if got := (secondarySet{Set: matchingBase, copies: transfers}).Match("www.transferred.example."); got != transferred {
		t.Fatal("status/generation update discarded current runtime data")
	}
	deleted, err := zone.NewSnapshot(4, true, base.Match("outside.test."), base.Match("home.example."))
	if err != nil {
		t.Fatal(err)
	}
	if got := (secondarySet{Set: deleted, copies: transfers}).Match("www.transferred.example."); got.Type() != v1alpha1.ZoneTypeForward || got.Name() != "." {
		t.Fatal("old transfer revived a deleted configuration")
	}
	// Invalid query configuration must publish denied coverage instead of
	// retaining the previous entire view. Check both an existing secondary
	// with old runtime data and a newly configured secondary under a root
	// forwarder, while an unrelated primary changes in the same generation.
	invalid := config.DeepCopy()
	invalid.Spec.Policy.QueryAccess = v1alpha1.AccessUseNetworkACL
	invalid.Spec.Policy.QueryACL = []string{"invalid-cidr"}
	added := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "invalid-new"}, Spec: v1alpha1.DNSZoneSpec{
		ZoneName: "new.example.", Type: v1alpha1.ZoneTypeSecondary,
		Secondary: &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.99"}},
		Policy:    v1alpha1.ZonePolicy{QueryAccess: "invalid-mode"},
	}}
	updatedPrimary := primary.DeepCopy()
	updatedPrimary.Spec.DefaultTTL = 99
	for _, obj := range []store.Object{invalid, added, updatedPrimary} {
		if _, err := st.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	for _, config := range []*v1alpha1.DNSZone{invalid, added} {
		if err := st.Get(ctx, store.Key{Kind: "DNSZone", Name: config.Name}, config); err != nil {
			t.Fatal(err)
		}
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		set := source.Zones()
		changedCopy := set.Match("www.transferred.example.")
		newCopy := set.Match("www.new.example.")
		changedPrimary := set.Match("home.example.")
		if changedCopy != nil && changedCopy != transferred && changedCopy.Type() == v1alpha1.ZoneTypeSecondary &&
			newCopy != nil && newCopy.Type() == v1alpha1.ZoneTypeSecondary && changedPrimary != nil && changedPrimary.SOA().Hdr.Ttl == 99 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("invalid secondary retained the old view or lost authority coverage")
		}
		time.Sleep(time.Millisecond)
	}
	for _, config := range []*v1alpha1.DNSZone{invalid, added} {
		current := source.Zones().Match("www." + config.Spec.ZoneName)
		if current.AvailableAt(now) || current.Serial() != 0 || current.SecondaryIdentity() != zone.SecondaryIdentity(config) || current.QueryAllowed(netip.MustParseAddr("192.0.2.1")) {
			t.Fatal("invalid configuration matched old runtime data or changed its original identity")
		}
		query := new(dns.Msg)
		query.SetQuestion("www."+config.Spec.ZoneName, dns.TypeA)
		request := &pipeline.Request{Msg: query, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:5000")}}
		called := false
		response, err := authoritative.New(source).Serve(request, func(r *pipeline.Request) (*pipeline.Response, error) {
			called = true
			return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
		})
		if err != nil || called || response.Msg.Rcode != dns.RcodeRefused || response.Status != pipeline.StatusRefusedACL {
			t.Fatalf("invalid secondary fell through root or failed open: %+v %v", response, err)
		}
	}
}
