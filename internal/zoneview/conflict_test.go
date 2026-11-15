package zoneview

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

func conflictZone(name, apex string, typ v1alpha1.ZoneType) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1alpha1.DNSZoneSpec{
		ZoneName: apex, Type: typ, DefaultTTL: 300,
		Secondary: &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}},
	}}
}

func conflictRecords(name, zoneName string, endpoints ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: zoneName}, Endpoints: endpoints}}
}

func conflictCache(t *testing.T, objects ...store.Object) (*memory.Store, *zonecache.Cache) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	st := memory.New()
	for _, object := range objects {
		if _, err := st.Apply(ctx, object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			cancel()
			_ = st.Close()
			t.Fatal(err)
		}
	}
	c, err := zonecache.New(st, zonecache.Options{Debounce: time.Millisecond})
	if err != nil {
		cancel()
		_ = st.Close()
		t.Fatal(err)
	}
	if err := c.Register(Builder(nil)); err != nil {
		cancel()
		_ = st.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
		_ = st.Close()
	})
	select {
	case <-c.Initialized():
	case <-time.After(5 * time.Second):
		t.Fatal("configuration cache did not initialize")
	}
	return st, c
}

func awaitConflictView(t *testing.T, c *zonecache.Cache, predicate func(*zone.Snapshot) bool) *zone.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if view, ok := zonecache.View[*zone.Snapshot](c.Snapshot(), ViewName); ok && view != nil && predicate(view) {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("expected conflict view was not published; degraded=%+v", c.Degraded())
	return nil
}

func queryConflict(t *testing.T, src authoritative.Source, name string, qtype uint16, noRD bool) (*pipeline.Response, int) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = !noRD
	req := &pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.1:5300"), Proto: pipeline.ProtoTCP}}
	calls := 0
	resp, err := authoritative.New(src).Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
		calls++
		return pipeline.Reply(r, dns.RcodeSuccess, pipeline.StatusForwarded), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp, calls
}

func assertConflictQueries(t *testing.T, src authoritative.Source, apex string, aliases ...string) {
	t.Helper()
	names := append([]string{apex, "www." + apex, "missing." + apex}, aliases...)
	for _, name := range names {
		for _, noRD := range []bool{false, true} {
			resp, calls := queryConflict(t, src, name, dns.TypeA, noRD)
			if resp.Msg.Rcode != dns.RcodeServerFailure || calls != 0 || len(resp.Msg.Answer) != 0 || len(resp.Msg.Ns) != 0 {
				t.Fatalf("conflicting %s RD=%v leaked data or forwarded: %+v, calls=%d", name, !noRD, resp, calls)
			}
		}
	}
	for _, qtype := range []uint16{dns.TypeSOA, dns.TypeAXFR, dns.TypeIXFR} {
		resp, calls := queryConflict(t, src, apex, qtype, false)
		if resp.Msg.Rcode != dns.RcodeServerFailure || calls != 0 || resp.Transfer != nil {
			t.Fatalf("conflicting %s query served or forwarded: %+v", dns.TypeToString[qtype], resp)
		}
	}
	if _, err := authoritative.New(src).Transfer(apex, 0); !errors.Is(err, authoritative.ErrUnsynced) {
		t.Fatalf("conflict coverage allowed transfer: %v", err)
	}
}

func TestDuplicateApicesReplacePreviousView(t *testing.T) {
	for _, existingPrimary := range []bool{false, true} {
		name := "new-secondary-under-root-forwarder"
		if existingPrimary {
			name = "convert-serving-primary-to-duplicate-secondary"
		}
		t.Run(name, func(t *testing.T) {
			root := conflictZone("root", ".", v1alpha1.ZoneTypeForward)
			fresh := conflictZone("fresh", "home.example.", v1alpha1.ZoneTypePrimary)
			copyConfig := conflictZone("copy", "copy.example.", v1alpha1.ZoneTypePrimary)
			objects := []store.Object{root, fresh, conflictRecords("fresh-records", fresh.Name,
				v1alpha1.Endpoint{DNSName: "www.home.example.", RecordType: "A", Targets: []string{"192.0.2.80"}},
				v1alpha1.Endpoint{DNSName: "alias.home.example.", RecordType: "CNAME", Targets: []string{"www.copy.example."}},
			)}
			if existingPrimary {
				objects = append(objects, copyConfig, conflictRecords("old-copy-records", copyConfig.Name,
					v1alpha1.Endpoint{DNSName: "www.copy.example.", RecordType: "A", Targets: []string{"203.0.113.9"}},
				))
			}
			st, c := conflictCache(t, objects...)
			src := Source{Cache: c}
			initial := awaitConflictView(t, c, func(view *zone.Snapshot) bool { return view.Match("www.home.example.") != nil })
			oldCopy := initial.Match("www.copy.example.")
			resp, calls := queryConflict(t, src, "www.copy.example.", dns.TypeA, false)
			if existingPrimary {
				if calls != 0 || len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].(*dns.A).A.String() != "203.0.113.9" {
					t.Fatal("initial primary was not serving the stale-positive control")
				}
			} else if calls != 1 || oldCopy.Type() != v1alpha1.ZoneTypeForward {
				t.Fatal("initial root forwarder was not the fallback control")
			}
			copyConfig.Spec.Type = v1alpha1.ZoneTypeSecondary
			duplicate := conflictZone("other-copy", "COPY.Example.", v1alpha1.ZoneTypeSecondary)
			duplicate.Spec.Policy.QueryAccess = v1alpha1.AccessDeny
			fresh.Spec.DefaultTTL = 99
			for _, object := range []store.Object{copyConfig, duplicate, fresh} {
				if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					t.Fatal(err)
				}
			}
			view := awaitConflictView(t, c, func(view *zone.Snapshot) bool {
				z := view.Match("www.copy.example.")
				home := view.Match("www.home.example.")
				return z != nil && z.Type() == v1alpha1.ZoneTypeSecondary && z.SecondaryIdentity() == "" &&
					home != nil && home.SOA() != nil && home.SOA().Hdr.Ttl == 99
			})
			if view.Match("www.copy.example.") == oldCopy || !src.Zones().Synced() {
				t.Fatal("duplicate apex retained the previous view or closed unrelated zones")
			}
			assertConflictQueries(t, src, "copy.example.", "alias.home.example.")
			resp, calls = queryConflict(t, src, "www.home.example.", dns.TypeA, false)
			if calls != 0 || len(resp.Msg.Answer) != 1 || resp.Msg.Answer[0].Header().Ttl != 99 {
				t.Fatal("unrelated primary did not publish its fresh data")
			}
			resp, calls = queryConflict(t, src, "outside.example.net.", dns.TypeA, false)
			if calls != 1 || resp.Msg.Rcode != dns.RcodeSuccess {
				t.Fatal("conflict disabled unrelated root forwarding")
			}
			if served := Served(c); len(served) != 1 || served[0].Name() != "home.example." {
				t.Fatalf("served listing includes conflict coverage: %v", served)
			}
			for _, condition := range c.Degraded() {
				if condition.Reason == zonecache.ReasonBuildFailed {
					t.Fatal("duplicate apex still failed the whole builder")
				}
			}
			// Removing the competing object restores ordinary placeholder
			// identity without reviving the primary's old object records.
			if err := st.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: duplicate.Name}, store.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
			awaitConflictView(t, c, func(view *zone.Snapshot) bool {
				z := view.Match("www.copy.example.")
				return z != nil && z.SecondaryIdentity() != "" && z.SOA() == nil
			})
			assertConflictQueries(t, src, "copy.example.", "alias.home.example.")
		})
	}
}

func TestDuplicateApicesCoverEveryZoneType(t *testing.T) {
	types := []v1alpha1.ZoneType{v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward, v1alpha1.ZoneTypeSecondary,
		v1alpha1.ZoneTypeStub, v1alpha1.ZoneTypeExternal, v1alpha1.ZoneTypeCatalog, v1alpha1.ZoneTypeSecondaryCatalog}
	objects := []store.Object{conflictZone("root", ".", v1alpha1.ZoneTypeForward)}
	for _, typ := range types {
		apex := string(typ) + ".example."
		first := conflictZone("first-"+string(typ), apex, typ)
		second := conflictZone("second-"+string(typ), apex, typ)
		first.Spec.Policy.QueryAccess = "invalid-policy"
		objects = append(objects, first, second)
	}
	_, c := conflictCache(t, objects...)
	view := awaitConflictView(t, c, func(view *zone.Snapshot) bool { return view.Len() == len(types)+1 })
	for _, typ := range types {
		apex := string(typ) + ".example."
		z := view.Match("www." + apex)
		if z == nil || z.Name() != dns.CanonicalName(apex) || z.SecondaryIdentity() != "" || z.AvailableAt(time.Now()) {
			t.Fatalf("conflict lost coverage for %s", typ)
		}
		assertConflictQueries(t, Source{Cache: c}, apex)
	}
	if len(Served(c)) != 0 {
		t.Fatal("conflict coverage is listed as served data")
	}
}
