package authoritative

import (
	"errors"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func secondaryStageZone(t testing.TB, loaded bool, expiry time.Time) *zone.Zone {
	t.Helper()
	dz := dnsZone("child.example.org.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeSecondary
		dz.Spec.Secondary = &v1alpha1.SecondarySpec{Primaries: []string{"192.0.2.53"}}
		dz.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow
		dz.Status.Serial = 123 // Must not replace the external serial.
	})
	var data []dns.RR
	if loaded {
		data = []dns.RR{
			mustRR("child.example.org. 7200 IN SOA ns.child.example.org. admin.child.example.org. 4294967295 3600 60 7200 90"),
			mustRR("child.example.org. 600 IN NS ns.child.example.org."),
			mustRR("www.child.example.org. 42 IN A 192.0.2.80"),
		}
	}
	z, err := zone.BuildSecondary(dz, data, expiry)
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func secondaryStage(t testing.TB, child *zone.Zone, now time.Time) *Stage {
	t.Helper()
	aliases := build(t, dnsZone("example.net."),
		ep("web.example.net", "A", "192.0.2.1"),
		ep("alias.example.net", "CNAME", "www.child.example.org"),
		ep("chain.example.net", "CNAME", "alias.example.net"),
	)
	parent := build(t, dnsZone("example.org."),
		ep("inside.example.org", "CNAME", "www.child.example.org"),
		ep("*.example.org", "A", "192.0.2.99"),
	)
	root := build(t, dnsZone(".", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Nameservers = nil
		dz.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "root-forwarder"}
	}))
	snap, err := zone.NewSnapshot(1, true, root, aliases, parent, child)
	if err != nil {
		t.Fatal(err)
	}
	holder := new(zone.Holder)
	holder.Store(snap)
	st := New(holder)
	st.Now = func() time.Time { return now }
	return st
}

func TestSecondaryUnavailableNeverFallsThrough(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	for _, state := range []struct {
		name   string
		loaded bool
		now    time.Time
	}{
		{"placeholder", false, expiry.Add(-time.Hour)},
		{"at-expiry", true, expiry},
		{"after-expiry", true, expiry.Add(time.Nanosecond)},
	} {
		t.Run(state.name, func(t *testing.T) {
			child := secondaryStageZone(t, state.loaded, expiry)
			st := secondaryStage(t, child, state.now)
			if !st.Ready() {
				t.Fatal("one unavailable secondary changed overall cache readiness")
			}
			for _, qname := range []string{"www.child.example.org.", "missing.child.example.org.", "alias.example.net.", "chain.example.net."} {
				for _, noRD := range []bool{false, true} {
					tc := Case{Qname: qname, Qtype: dns.TypeA, NoRD: noRD}
					rec := &recorder{}
					resp, err := st.Serve(tc.request(), rec.next)
					if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Status != pipeline.StatusServfailUnsynced || len(rec.calls) != 0 {
						t.Fatalf("%s RD=%v: resp=%+v err=%v next=%d", qname, !noRD, resp, err, len(rec.calls))
					}
					if resp.Upstream != "secondary:unavailable:child.example.org." || len(resp.Msg.Answer) != 0 || len(resp.Msg.Ns) != 0 {
						t.Fatalf("unavailable response leaks a partial chain or false denial: %+v", resp)
					}
				}
			}
			for _, qtype := range []uint16{dns.TypeSOA, dns.TypeAAAA, dns.TypeAXFR, dns.TypeIXFR} {
				tc := Case{Qname: "child.example.org.", Qtype: qtype, Proto: pipeline.ProtoTCP}
				rec := &recorder{}
				resp, err := st.Serve(tc.request(), rec.next)
				if err != nil || resp.Msg.Rcode != dns.RcodeServerFailure || len(rec.calls) != 0 || resp.Transfer != nil {
					t.Fatalf("unavailable %s query: %+v %v", dns.TypeToString[qtype], resp, err)
				}
			}
			if _, err := st.Transfer(child.Name(), 0); !errors.Is(err, ErrUnsynced) {
				t.Fatalf("unavailable transfer provider returned %v", err)
			}
			for _, qname := range []string{"web.example.net.", "unrelated.test."} {
				tc := Case{Qname: qname, Qtype: dns.TypeA}
				rec := &recorder{}
				resp, err := st.Serve(tc.request(), rec.next)
				if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || len(resp.Msg.Answer) != 1 {
					t.Fatalf("unrelated %s: %+v %v", qname, resp, err)
				}
				if qname == "web.example.net." {
					if len(rec.calls) != 0 || resp.Status != pipeline.StatusAuthoritative {
						t.Fatal("unrelated primary stopped serving")
					}
				} else if len(rec.calls) != 1 || rec.calls[0].UpstreamGroup != "root-forwarder" {
					t.Fatal("unrelated forwarding changed")
				}
			}
		})
	}
}

func TestSecondaryAnswersImmediatelyBeforeExpiry(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	st := secondaryStage(t, secondaryStageZone(t, true, expiry), expiry.Add(-time.Nanosecond))
	for _, qname := range []string{"www.child.example.org.", "alias.example.net.", "chain.example.net."} {
		tc := Case{Qname: qname, Qtype: dns.TypeA}
		rec := &recorder{}
		resp, err := st.Serve(tc.request(), rec.next)
		if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || resp.Status != pipeline.StatusAuthoritative || len(rec.calls) != 0 {
			t.Fatalf("available %s: %+v %v", qname, resp, err)
		}
		last := resp.Msg.Answer[len(resp.Msg.Answer)-1]
		if last.(*dns.A).A.String() != "192.0.2.80" || last.Header().Ttl != 42 {
			t.Fatal("secondary answer did not preserve transferred data")
		}
	}
	for _, tc := range []Case{{Qname: "child.example.org.", Qtype: dns.TypeSOA}, {Qname: "www.child.example.org.", Qtype: dns.TypeAAAA}, {Qname: "missing.child.example.org.", Qtype: dns.TypeA}} {
		rec := &recorder{}
		resp, err := st.Serve(tc.request(), rec.next)
		if err != nil || len(rec.calls) != 0 {
			t.Fatalf("secondary SOA response: %+v %v", resp, err)
		}
		soaRecords := resp.Msg.Ns
		if tc.Qtype == dns.TypeSOA {
			soaRecords = resp.Msg.Answer
		}
		if len(soaRecords) != 1 || soaRecords[0].(*dns.SOA).Serial != 4294967295 {
			t.Fatal("external SOA serial was restamped in authoritative response")
		}
	}
}

// A CNAME can stay inside an enclosing primary's name space while crossing
// into a more-specific configured zone. Parent wildcard synthesis must not
// bypass the child's availability or substitute parent data for a live copy.
func TestSecondaryMoreSpecificCNAMETarget(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	for _, state := range []struct {
		name   string
		loaded bool
		now    time.Time
		want   int
	}{
		{"placeholder", false, expiry.Add(-time.Hour), dns.RcodeServerFailure},
		{"at-expiry", true, expiry, dns.RcodeServerFailure},
		{"available", true, expiry.Add(-time.Nanosecond), dns.RcodeSuccess},
	} {
		t.Run(state.name, func(t *testing.T) {
			st := secondaryStage(t, secondaryStageZone(t, state.loaded, expiry), state.now)
			tc := Case{Qname: "inside.example.org.", Qtype: dns.TypeA}
			rec := &recorder{}
			resp, err := st.Serve(tc.request(), rec.next)
			if err != nil || len(rec.calls) != 0 || resp.Msg.Rcode != state.want {
				t.Fatalf("CNAME bypassed more-specific child: resp=%+v err=%v next=%d", resp, err, len(rec.calls))
			}
			if state.want == dns.RcodeSuccess {
				if len(resp.Msg.Answer) != 2 || resp.Msg.Answer[1].(*dns.A).A.String() != "192.0.2.80" || resp.Msg.Answer[1].Header().Ttl != 42 {
					t.Fatalf("parent data replaced transferred child answer: %v", resp.Msg.Answer)
				}
			} else if len(resp.Msg.Answer) != 0 || len(resp.Msg.Ns) != 0 {
				t.Fatal("unavailable child leaked a parent answer or denial")
			}
		})
	}
}

func TestSecondaryStaleConfigurationRemainsConservative(t *testing.T) {
	expiry := time.Now().Add(time.Hour)
	child := secondaryStageZone(t, true, expiry)
	st := secondaryStage(t, child, expiry.Add(-time.Nanosecond))
	holder := st.Source.(*zone.Holder)
	holder.Store(holder.Load().WithGate(zone.GateStale))
	for _, qname := range []string{"www.child.example.org.", "missing.child.example.org.", "alias.example.net."} {
		tc := Case{Qname: qname, Qtype: dns.TypeA}
		rec := &recorder{}
		resp, err := st.Serve(tc.request(), rec.next)
		if err != nil || len(rec.calls) != 0 || resp.Msg.Rcode != dns.RcodeServerFailure || resp.Upstream != UpstreamStale {
			t.Fatalf("stale configuration served a secondary: %+v %v", resp, err)
		}
	}
	if _, err := st.Transfer(child.Name(), 0); !errors.Is(err, ErrUnsynced) {
		t.Fatalf("stale configuration allowed transfer: %v", err)
	}
	tc := Case{Qname: "unrelated.test.", Qtype: dns.TypeA}
	rec := &recorder{}
	resp, err := st.Serve(tc.request(), rec.next)
	if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || len(rec.calls) != 1 {
		t.Fatal("stale configuration blocked unrelated forwarding")
	}
}
