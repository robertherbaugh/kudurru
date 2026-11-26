package primaryruntime

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSQLiteUpdateViewOwnership(t *testing.T) {
	for _, namespace := range []string{"default", "kids"} {
		t.Run(namespace, func(t *testing.T) {
			h := newHarness(t, "sqlite")
			ctx := t.Context()
			lifecycle := h.begin(h.zone("primary", "example."))
			h.r.o.Proofs = testIssuer{}
			applyView := func(owner string) {
				t.Helper()
				set := &public.DNSRecordSet{
					ObjectMeta: metav1.ObjectMeta{Name: owner, Namespace: namespace},
					Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "primary"}, Endpoints: []public.Endpoint{{
						DNSName: owner + ".example.", RecordType: "A", Targets: []string{"192.0.2.20"}, Kudurru: &public.EndpointExt{View: "lan"},
					}}},
				}
				if _, err := h.st.Apply(ctx, set, store.ApplyOptions{Manager: "api"}); err != nil {
					t.Fatal(err)
				}
			}
			reconcile := func() *zonestate.View {
				t.Helper()
				view, err := h.r.Reconcile(ctx, lifecycle.Spec.Zone)
				if err != nil {
					t.Fatal(err)
				}
				return view
			}
			check := func(name, defaultIP, groupIP string) {
				t.Helper()
				model := h.r.Capture(time.Now()).Match(name)
				if model == nil {
					t.Fatal("zone not published")
				}
				for _, answer := range []struct {
					view *zone.Zone
					ip   string
				}{
					{model, defaultIP}, {model.ForGroups([]string{namespace + "/lan"}), groupIP},
				} {
					got := answer.view.Lookup(name, dns.TypeA)
					if answer.ip == "" && got.Kind == zone.NXDomain {
						continue
					}
					if got.Kind != zone.Success || len(got.Answer) != 1 || got.Answer[0].(*dns.A).A.String() != answer.ip {
						t.Fatalf("%s: %v %v, want %q (empty means NXDOMAIN)", name, got.Kind, got.Answer, answer.ip)
					}
				}
			}
			applyView("printer")
			before := reconcile()
			check("printer.example.", "", "192.0.2.20")
			request := h.request(lifecycle, "reserved", "printer.example. 60 IN A 192.0.2.21", false)
			if out := h.r.Apply(ctx, request); out.Rcode != dns.RcodeRefused || out.Committed {
				t.Fatalf("UPDATE of reserved view name: %+v", out)
			}
			after, err := h.repo.Read(ctx, lifecycle.Spec.HeadZone())
			if err != nil || after.Head.Spec != before.Head.Spec {
				t.Fatal("refusal changed the committed head", err)
			}
			check("printer.example.", "", "192.0.2.20")

			control := h.request(lifecycle, "unreserved", "lease.example. 60 IN A 192.0.2.21", false)
			if out := h.r.Apply(ctx, control); out.Rcode != dns.RcodeSuccess || !out.Published {
				t.Fatalf("UPDATE of unreserved control name: %+v", out)
			}
			check("lease.example.", "192.0.2.21", "192.0.2.21")
			applyView("lease")
			reconcile()
			check("lease.example.", "192.0.2.21", "192.0.2.21")
			h.r.o.Compiler = primaryzone.NewCompiler(primaryzone.CompilerOptions{
				Takeover: func(context.Context, *zonestate.View, *zonestate.DesiredInputs) (bool, error) { return true, nil },
			})
			reconcile()
			check("lease.example.", "", "192.0.2.20")
		})
	}
}
