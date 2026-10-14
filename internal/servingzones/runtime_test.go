package servingzones

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Explicit local test receiver/discoverer. Native signed transport acquisition
// and offline restore are tested separately in secondary/rpz binding tests.
type copyReceiver struct{}

func copyRecords(z *public.DNSZone) []dns.RR {
	apex := z.Spec.ZoneName
	records := []dns.RR{
		&dns.SOA{Hdr: dns.RR_Header{Name: apex, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60}, Ns: "ns." + apex, Mbox: "admin." + apex, Serial: 1, Refresh: 10, Retry: 3, Expire: 30, Minttl: 5},
		&dns.NS{Hdr: dns.RR_Header{Name: apex, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 30}, Ns: "ns." + apex},
		&dns.A{Hdr: dns.RR_Header{Name: "ns." + apex, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: []byte{192, 0, 2, 53}},
	}
	if apex == "policy." {
		records = append(records, &dns.CNAME{Hdr: dns.RR_Header{Name: "bad.test." + apex, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 30}, Target: "."})
	}
	return records
}
func (copyReceiver) SOA(_ context.Context, z *public.DNSZone, _ string) (*dns.SOA, error) {
	return copyRecords(z)[0].(*dns.SOA), nil
}
func (copyReceiver) AXFR(_ context.Context, z *public.DNSZone, _ string) ([]dns.RR, error) {
	return copyRecords(z), nil
}

type discovery struct{ now time.Time }

func (d discovery) Discover(_ context.Context, z *public.DNSZone) (*zone.StubData, error) {
	return &zone.StubData{Records: copyRecords(z), Servers: []zone.StubServer{{Name: "ns." + z.Spec.ZoneName, Address: netip.MustParseAddrPort("192.0.2.53:53"), Protocol: "udp"}}, ConfirmedAt: d.now, ExpiresAt: d.now.Add(20 * time.Second)}, nil
}
func bindingKeys(t *testing.T, secret string) *dnsserver.StaticKeyring {
	t.Helper()
	keys, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "copy-key.", Algorithm: dns.HmacSHA256, Secret: []byte(secret)})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
func startBackground(t *testing.T, run func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("background shutdown did not finish")
		}
	})
}
func waitRuntime(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runtime copies were not published")
}

func TestActualManagersComposePinnedRuntimeRotationAndExpiry(t *testing.T) {
	f := newFixture(t)
	secondaryConfig := f.config("secondary", "secondary.", public.ZoneTypeSecondary)
	secondaryConfig.Spec.Secondary.TSIGKeyRef = "copy-key."
	secondaryConfig = f.apply(secondaryConfig).(*public.DNSZone)
	stubConfig := f.config("stub", "stub.", public.ZoneTypeStub)
	list := f.apply(&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "ads", Namespace: "apps"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.", Primaries: []string{"127.0.0.1:5300"}, TSIGKeyRef: "copy-key."}}}}}).(*public.BlockList)
	keys := bindingKeys(t, "copy-test-key-one")
	cache, err := zonecache.New(f.store, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startBackground(t, cache.Start)
	select {
	case <-cache.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("cache did not initialize")
	}
	db, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sm, err := secondary.New(secondary.Options{Cache: cache, Blobs: db, ClusterID: f.store.Info().ClusterID, Keyring: keys, Receiver: copyReceiver{}, Now: func() time.Time { return f.now }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	stubManager, err := stub.New(stub.Options{Cache: cache, Blobs: db, ClusterID: f.store.Info().ClusterID, Discoverer: discovery{f.now}, Now: func() time.Time { return f.now }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	rm, err := rpz.New(rpz.Options{Cache: cache, Blobs: db, ClusterID: f.store.Info().ClusterID, Keyring: keys, Receiver: copyReceiver{}, Now: func() time.Time { return f.now }, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	startBackground(t, sm.Run)
	startBackground(t, stubManager.Run)
	startBackground(t, rm.Run)
	waitRuntime(t, func() bool {
		return sm.Snapshot().Lookup(zone.SecondaryIdentity(secondaryConfig)) != nil && stubManager.Snapshot().Lookup(zone.StubIdentity(stubConfig)) != nil && rm.Snapshot().Lookup(rpz.SourceIdentity(list, 0)) != nil
	})
	o := f.options(f.image())
	o.Keys, o.Secondary, o.Stub, o.RPZ = keys, sm.Snapshot(), stubManager.Snapshot(), rm.Snapshot()
	p := mustPrepare(t, o)
	if !p.Zones(f.now).Match("ns.secondary.").AvailableAt(f.now) || !p.Zones(f.now).Match("ns.stub.").RoutingAvailableAt(f.now) {
		t.Fatal("bound runtime did not replace placeholders")
	}
	selection, err := p.SelectRPZ([]string{"apps/ads"})
	if err != nil || selection.Evaluate(rpz.Context{Name: "bad.test."}, f.now).Action != rpz.ActionNXDOMAIN {
		t.Fatal("pinned RPZ unavailable", err)
	}
	unavailable(t, p, "stub.", f.now.Add(20*time.Second))
	unavailable(t, p, "secondary.", f.now.Add(30*time.Second))
	if !selection.Evaluate(rpz.Context{Name: "bad.test."}, f.now.Add(30*time.Second)).Unavailable {
		t.Fatal("exact RPZ expiry renewed")
	}
	rotated := o
	rotated.Keys = bindingKeys(t, "copy-test-key-two")
	newCut := mustPrepare(t, rotated)
	unavailable(t, newCut, "secondary.", f.now)
	rotatedSelection, err := newCut.SelectRPZ([]string{"apps/ads"})
	if err != nil || !rotatedSelection.Evaluate(rpz.Context{Name: "bad.test."}, f.now).Unavailable {
		t.Fatal("rotation relabeled RPZ", err)
	}
	if !p.Zones(f.now).Match("ns.secondary.").AvailableAt(f.now) || selection.Evaluate(rpz.Context{Name: "bad.test."}, f.now).Action != rpz.ActionNXDOMAIN {
		t.Fatal("rotation mutated held cut")
	}
	// Existing durable fingerprint semantics intentionally accept restored
	// identical bytes, not a claimed fresh acquisition epoch or ABA guarantee.
	restored := o
	restored.Keys = bindingKeys(t, "copy-test-key-one")
	if !mustPrepare(t, restored).Zones(f.now).Match("ns.secondary.").AvailableAt(f.now) {
		t.Fatal("restored same bytes violated durable equality contract")
	}
	for _, desired := range o.Objects.Zones() {
		if desired.Spec.Type == public.ZoneTypeSecondary {
			desired.UID = "recreated"
		}
		if desired.Spec.Type == public.ZoneTypeStub {
			desired.Spec.Stub.Nameservers = []string{"127.0.0.1:5301"}
		}
	}
	changed := mustPrepare(t, o)
	unavailable(t, changed, "secondary.", f.now)
	unavailable(t, changed, "stub.", f.now)
}
