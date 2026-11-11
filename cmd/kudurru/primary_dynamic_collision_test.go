package main

import (
	"context"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPrimaryRuntimeRecordSetAtDynamicNameKeepsZoneCommitting writes a
// DNSRecordSet at a name a signed dynamic update already owns, on the
// mounted primary runtime with a real SQLite store. The colliding endpoint
// is left out and reported, the other endpoint of its record set and the
// dynamic record keep answering, and a record set written afterwards is
// committed and answered. Before D373 the compiler
// refused the whole projection for the conflict, so the zone took no
// change at all while the dynamic record existed.
func TestPrimaryRuntimeRecordSetAtDynamicNameKeepsZoneCommitting(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	answer, err := f.exchange(primaryUpdateMessage("example.", "dyn.example.", true), false)
	if err != nil || answer.Rcode != dns.RcodeSuccess {
		t.Fatal("signed UPDATE failed", answer, err)
	}
	lookup := func(name string) string {
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer, err := f.exchange(query, false)
		if err != nil || !answer.Authoritative || len(answer.Answer) != 1 {
			return ""
		}
		a, _ := answer.Answer[0].(*dns.A)
		if a == nil {
			return ""
		}
		return a.A.String()
	}
	set := func(name, host, address string) *public.DNSRecordSet {
		return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: host, RecordType: "A", Targets: []string{address}}}}}
	}
	apply := func(objects ...store.Object) {
		t.Helper()
		if err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			for _, obj := range objects {
				if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply(set("collides", "dyn.example.", "192.0.2.40"))
	// A record set with one endpoint at the dynamic name and one elsewhere
	// keeps serving the other endpoint: only the colliding endpoint is
	// left out.
	mixed := set("mixed", "kept.example.", "192.0.2.42")
	mixed.Spec.Endpoints = append(mixed.Spec.Endpoints, public.Endpoint{DNSName: "dyn.example.", RecordType: "A", Targets: []string{"192.0.2.43"}})
	apply(mixed)
	apply(set("later", "later.example.", "192.0.2.41"))
	f.wait(t, func() bool { return lookup("later.example.") == "192.0.2.41" })
	f.wait(t, func() bool { return lookup("kept.example.") == "192.0.2.42" })
	if got := lookup("dyn.example."); got != "192.0.2.33" {
		t.Fatalf("dyn.example. = %q, the dynamic update stored 192.0.2.33", got)
	}
}
