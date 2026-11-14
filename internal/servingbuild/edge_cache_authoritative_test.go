package servingbuild

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAuthoritativeCacheFrozenPrimaryACLAndImportedSignatures(t *testing.T) {
	for _, tc := range []struct {
		name, extra, reason string
		policy              public.ZonePolicy
	}{
		{name: "uniform-allow"},
		{name: "uniform-deny", policy: public.ZonePolicy{QueryAccess: public.AccessDeny}},
		{name: "frozen-client-acl", policy: public.ZonePolicy{QueryAccess: public.AccessUseNetworkACL, QueryACL: []string{"192.0.2.1/32"}}, reason: "zone-client-acl"},
		{name: "nameserver-identity", policy: public.ZonePolicy{QueryAccess: public.AccessAllowOnlyZoneNameServers}, reason: "zone-client-acl"},
		{name: "private-client", policy: public.ZonePolicy{QueryAccess: public.AccessAllowOnlyPrivate}, reason: "zone-client-acl"},
		{name: "raw-signature", extra: "name.static.example. 60 IN RRSIG A 13 3 60 20270101000000 20260101000000 1234 static.example. AQID", reason: "signed-zone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "static"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "static.example.", Policy: tc.policy}}
			var records []dns.RR
			for _, text := range []string{"static.example. 60 IN SOA ns.static.example. admin.static.example. 1 60 30 600 30", "name.static.example. 60 IN A 192.0.2.1", tc.extra} {
				if text == "" {
					continue
				}
				rr, err := dns.NewRR(text)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, rr)
			}
			model, err := zone.BuildCommittedPrimary(config, records, 1, time.Time{}, false)
			if err != nil {
				t.Fatal(err)
			}
			// A later desired allow policy must not repair the original model.
			config.Spec.Policy = public.ZonePolicy{QueryAccess: public.AccessAllow}
			if got := authoritativeModelBypass(model); got != tc.reason {
				t.Fatalf("original model proof = %q, want %q", got, tc.reason)
			}
		})
	}
}

// A served model with client-group views is refused even when no desired
// record set names a view any more (D403).
func TestAuthoritativeCacheRefusesServedClientViews(t *testing.T) {
	config := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "static"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "static.example."}}
	var records []dns.RR
	for _, text := range []string{"static.example. 60 IN SOA ns.static.example. admin.static.example. 1 60 30 600 30", "name.static.example. 60 IN A 192.0.2.1"} {
		rr, err := dns.NewRR(text)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rr)
	}
	view, err := dns.NewRR("name.static.example. 60 IN A 10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	model, err := zone.BuildCommittedPrimaryWithViews(config, records, []zone.CommittedView{{Group: "default/lan", Records: []dns.RR{view}}}, 1, time.Time{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := authoritativeModelBypass(model); got != "zone-client-view" {
		t.Fatalf("model proof = %q, want zone-client-view", got)
	}
}

func TestAuthoritativeCacheOriginalStaticAndUnavailableSources(t *testing.T) {
	f := newFixture(t)
	settings := settings("192.0.2.11")
	settings.Spec.Settings["ratelimit"] = extensions.JSON{Raw: []byte(`{"enabled":false}`)}
	if _, err := f.store.Apply(t.Context(), settings, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	edge, err := NewEdgeCache(f.pub, "original-node", func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	before, err := edge.State(t.Context())
	if err != nil || !before.Available || before.AuthoritativeAvailable || before.AuthoritativeReason != "zone-unavailable" {
		t.Fatal("uncommitted original authority qualified")
	}
	// These detached original inputs are refused even before a later model
	// starts serving a client view or an expiring contribution.
	for _, tc := range []struct {
		reason string
		ext    *public.EndpointExt
	}{
		{"zone-expiry", &public.EndpointExt{ExpiresAt: &metav1.Time{Time: f.now.Add(time.Minute)}}},
		{"zone-client-view", &public.EndpointExt{View: "specific-client-group"}},
	} {
		records := &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "unsafe", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "uncommitted"}, Endpoints: []public.Endpoint{{DNSName: "name.uncommitted.example.", RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: tc.ext}}}}
		if _, err := f.store.Apply(t.Context(), records, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		f.waitCurrent(t)
		state, err := edge.State(t.Context())
		if err != nil || state.AuthoritativeAvailable || state.AuthoritativeReason != tc.reason {
			t.Fatal("unsafe original record contribution qualified", tc.reason)
		}
		// The edge packet cache keys on the question only: a client-group
		// view must also keep the resolver pool from caching (D403).
		if tc.reason == "zone-client-view" && (state.Available || state.UnavailableReason != tc.reason) {
			t.Fatal("client-group view left the resolver pool cache available")
		}
	}
	if err := f.store.Delete(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "unsafe"}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	if err := f.store.Delete(t.Context(), store.Key{Kind: "DNSZone", Name: "uncommitted"}, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	forward := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "static"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypeForward, ZoneName: "static.example."}}
	if _, err := f.store.Apply(t.Context(), forward, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	held, err := f.pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	uniform, err := edge.State(t.Context())
	if err != nil || !uniform.Available || !uniform.AuthoritativeAvailable || uniform.AuthoritativeReason != "" {
		t.Fatal("original static uniform cut did not qualify")
	}
	forward.Spec.Policy.QueryAccess = public.AccessUseNetworkACL
	forward.Spec.Policy.QueryACL = []string{"192.0.2.1/32"}
	if _, err := f.store.Apply(t.Context(), forward, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	denied, err := edge.State(t.Context())
	if err != nil || denied.Available || denied.AuthoritativeAvailable || denied.AuthoritativeReason != "zone-client-acl" {
		t.Fatal("original client ACL did not refuse both pools")
	}
	old, err := edge.project(held, f.now)
	if err != nil || !old.AuthoritativeAvailable || old.Publication != uniform.Publication {
		t.Fatal("new desired ACL replaced held original evidence")
	}
	forward.Spec.Policy = public.ZonePolicy{}
	forward.Spec.Type, forward.Spec.Forward = public.ZoneTypeSecondary, nil
	forward.Spec.Secondary = &public.SecondarySpec{Primaries: []string{"192.0.2.53:53"}}
	if _, err := f.store.Apply(t.Context(), forward, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	runtime, err := edge.State(t.Context())
	if err != nil || runtime.AuthoritativeAvailable || runtime.AuthoritativeReason != "zone-runtime" {
		t.Fatal("unproven acquired runtime qualified")
	}
}
