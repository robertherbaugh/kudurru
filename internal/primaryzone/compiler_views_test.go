package primaryzone

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func viewEndpoint(name, target, view string) public.Endpoint {
	e := endpoint(name, target)
	e.Kudurru = &public.EndpointExt{View: view}
	return e
}

func TestCompilerSummarizesRefusedViewsInServedSets(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blockers []*public.DNSRecordSet
		refused  public.Endpoint
		want     string
		signed   bool
		viewOnly bool
	}{
		{"duplicate", []*public.DNSRecordSet{desiredSet("first", viewEndpoint("printer", "192.0.2.1", "lan"))}, viewEndpoint("printer", "192.0.2.2", "lan"), "Duplicated", false, true},
		{"occluded", []*public.DNSRecordSet{desiredSet("first", public.Endpoint{DNSName: "child.example.", RecordType: "NS", Targets: []string{"ns.other.test."}})}, viewEndpoint("printer.child", "192.0.2.2", "lan"), "Occluded", false, false},
		{"signed", nil, viewEndpoint("printer", "192.0.2.2", "lan"), "Invalid: client-group views are not served in a DNSSEC-signed zone", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := desiredZone()
			if tc.signed {
				z.Spec.DNSSEC = &public.DNSSECSpec{Enabled: true}
			}
			kept := endpoint("kept", "192.0.2.3")
			if tc.viewOnly {
				kept = viewEndpoint("kept", "192.0.2.3", "lan")
			}
			mixed := desiredSet("mixed", tc.refused, kept)
			in := &zonestate.DesiredInputs{Zone: z, RecordSets: append(tc.blockers, mixed), Complete: true}
			compiled := compile(t, NewCompiler(CompilerOptions{}), blank(z), in, fixedNow)
			if got := built(t, compiled).ForGroups([]string{"dns/lan"}).Lookup("kept.example.", dns.TypeA); got.Kind != zone.Success {
				t.Fatalf("other endpoint was lost: %+v", got)
			}
			reason := servedViewRefusal(t, compiled, mixed)
			if want := tc.refused.DNSName + " A (view dns/lan): " + tc.want; !strings.Contains(reason, want) {
				t.Fatalf("reason %q lacks %q", reason, want)
			}
		})
	}
}

func TestCompilerSummarizesSharedViewBudgetAcrossNamespaces(t *testing.T) {
	z := desiredZone()
	first := desiredSet("first")
	first.Namespace = "ahead"
	for index := range zone.MaxViews {
		first.Spec.Endpoints = append(first.Spec.Endpoints, viewEndpoint(fmt.Sprintf("first-%02d", index), "192.0.2.1", fmt.Sprintf("view-%02d", index)))
	}
	mixed := desiredSet("mixed", endpoint("kept", "192.0.2.3"))
	mixed.Namespace = "kids"
	for index := range 20 {
		mixed.Spec.Endpoints = append(mixed.Spec.Endpoints, viewEndpoint(fmt.Sprintf("printer-%02d", index), "192.0.2.2", fmt.Sprintf("view-%02d", index)))
	}
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{mixed, first}, Complete: true}
	c := NewCompiler(CompilerOptions{})
	compiled := compile(t, c, blank(z), in, fixedNow)
	model := built(t, compiled)
	groups := model.ViewGroups()
	if len(groups) != zone.MaxViews || groups[0] != "ahead/view-00" || groups[len(groups)-1] != "ahead/view-15" {
		t.Fatalf("wrong view budget winners: %v", groups)
	}
	if got := model.ForGroups([]string{"kids/view-00"}).Lookup("printer-00.example.", dns.TypeA); got.Kind != zone.NXDomain {
		t.Fatalf("over-budget view served: %+v", got)
	}
	reason := servedViewRefusal(t, compiled, mixed)
	if !strings.Contains(reason, "printer-00.example. A (view kids/view-00): Invalid: a zone serves at most 16 client-group views") ||
		!strings.HasSuffix(reason, " more") || len(reason) > 512 || !utf8.ValidString(reason) {
		t.Fatalf("unexpected bounded reason (%d bytes): %q", len(reason), reason)
	}
	if got := servedViewRefusal(t, compile(t, c, compiled, in, fixedNow), mixed); got != reason {
		t.Fatalf("reason changed on identical recompile: %q != %q", got, reason)
	}
}

func servedViewRefusal(t *testing.T, view *zonestate.View, rs *public.DNSRecordSet) string {
	t.Helper()
	for _, source := range view.Aggregate.Sources {
		if source.ID != store.KeyOf(rs).String() {
			continue
		}
		envelope, err := zonestate.DecodeSourceEnvelope(source.Desired)
		if err != nil || envelope.FormatVersion != 1 || envelope.Projection != "served" || envelope.Reason == "" {
			t.Fatalf("expected a served source with a refusal reason, got %+v (%v)", envelope, err)
		}
		return envelope.Reason
	}
	t.Fatal("missing source for record set")
	return ""
}

// The committed primary (SQLite) keeps client-group views apart from the
// default records: they survive the codec, build into per-group variants,
// stay out of the UPDATE state and change the serial when they change.
func TestCompilerCommitsClientGroupViews(t *testing.T) {
	z := desiredZone()
	split := desiredSet("split", endpoint("www", "203.0.113.10"), viewEndpoint("www", "10.0.0.10", "lan"), viewEndpoint("nas", "10.0.0.20", "lan"), viewEndpoint("www", "10.9.0.10", "kids"))
	hidden := desiredSet("hidden", public.Endpoint{DNSName: "sub.example.", RecordType: "NS", Targets: []string{"ns.example.net."}}, viewEndpoint("host.sub", "10.0.0.99", "lan"))
	only := desiredSet("only", viewEndpoint("printer", "10.0.0.40", "lan"))
	c := NewCompiler(CompilerOptions{})
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{split, hidden, only}, Complete: true}
	result, err := c.Project(context.Background(), blank(z), in, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]zonestate.SourceDiagnostic{}
	for _, d := range result.Diagnostics {
		outcomes[d.SourceID] = d
	}
	if o := outcomes["DNSRecordSet/dns/only"]; o.Outcome != "served" {
		t.Fatalf("a view-only record set is served to its group: %+v", o)
	}
	if o := outcomes["DNSRecordSet/dns/hidden"]; o.Outcome != "served" {
		t.Fatalf("hidden record set still serves its NS: %+v", o)
	}
	v := prepareView(t, result.Aggregate, nil)
	if len(v.Aggregate.Views) != 2 || v.Aggregate.Views[0].Group != "dns/kids" || v.Aggregate.Views[1].Group != "dns/lan" || len(v.Aggregate.Views[1].Records) != 3 {
		t.Fatalf("committed views %+v", v.Aggregate.Views)
	}
	for _, r := range v.Aggregate.Records {
		rr, _, _ := dns.UnpackRR(r.Wire, 0)
		if strings.Contains(rr.String(), "10.0.0.") || strings.Contains(rr.String(), "10.9.0.") {
			t.Fatalf("view data in the default records: %s", rr)
		}
	}
	model := built(t, v)
	answer := func(m *zone.Zone, name string) string {
		res := m.Lookup(name, dns.TypeA)
		if res.Kind != zone.Success {
			return res.Kind.String()
		}
		return res.Answer[len(res.Answer)-1].(*dns.A).A.String()
	}
	for _, tc := range []struct {
		groups []string
		name   string
		want   string
	}{
		{nil, "www.example.", "203.0.113.10"},
		{[]string{"dns/lan"}, "www.example.", "10.0.0.10"},
		{[]string{"dns/kids"}, "www.example.", "10.9.0.10"},
		{nil, "nas.example.", "nxdomain"},
		{[]string{"dns/lan"}, "nas.example.", "10.0.0.20"},
		{[]string{"dns/lan"}, "printer.example.", "10.0.0.40"},
		{[]string{"dns/kids"}, "printer.example.", "nxdomain"},
		{[]string{"dns/lan"}, "host.sub.example.", "delegation"},
	} {
		if got := answer(model.ForGroups(tc.groups), tc.name); got != tc.want {
			t.Errorf("%v %s = %s, want %s", tc.groups, tc.name, got, tc.want)
		}
	}
	state, err := v.UpdateState()
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range state.Records {
		if strings.Contains(rr.String(), "10.0.0.") {
			t.Fatalf("UPDATE prerequisites see view data: %s", rr)
		}
	}
	reserved := false
	for _, r := range state.Reservations {
		reserved = reserved || r.Name == "printer.example." && r.Type == dns.TypeA
	}
	if !reserved {
		t.Fatal("a view endpoint must reserve its name against dynamic updates")
	}
	// Recompiling the same input keeps the serial; changing only a view
	// advances it.
	again, err := c.Project(context.Background(), v, in, fixedNow.Add(10))
	if err != nil {
		t.Fatal(err)
	}
	same := prepareView(t, again.Aggregate, v.Head)
	if same.Head.Spec.Serial != v.Head.Spec.Serial {
		t.Fatal("unchanged views changed the serial")
	}
	changed := desiredSet("only", viewEndpoint("printer", "10.0.0.41", "lan"))
	changed.ResourceVersion = "only-2"
	in2 := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{split, hidden, changed}, Complete: true}
	next, err := c.Project(context.Background(), v, in2, fixedNow.Add(3600e9))
	if err != nil {
		t.Fatal(err)
	}
	moved := prepareView(t, next.Aggregate, v.Head)
	if moved.Head.Spec.Serial == v.Head.Spec.Serial || moved.Head.Spec.ContentHash == v.Head.Spec.ContentHash {
		t.Fatal("a view change must advance the serial")
	}
	if got := answer(built(t, moved).ForGroups([]string{"dns/lan"}), "printer.example."); got != "10.0.0.41" {
		t.Fatalf("changed view %s", got)
	}
}

// withDynamic commits base data, then adds one dynamic (RFC 2136) record to
// the committed aggregate as an UPDATE would, and returns the new head.
func withDynamic(t *testing.T, c *Compiler, z *public.DNSZone, in *zonestate.DesiredInputs, dynamic string) *zonestate.View {
	t.Helper()
	v := compile(t, c, blank(z), in, fixedNow)
	rr, err := dns.NewRR(dynamic)
	if err != nil {
		t.Fatal(err)
	}
	r, err := zonestate.FromRR(rr, zonestate.Provenance{Key: "key.", CredentialVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	a := *v.Aggregate
	a.Records = append(append([]zonestate.Record(nil), a.Records...), r)
	for i, r := range a.Records {
		rr, _, _ := dns.UnpackRR(r.Wire, 0)
		if soa, ok := rr.(*dns.SOA); ok {
			soa.Serial++
			a.Records[i], _ = zonestate.FromRR(soa, r.Provenance)
		}
	}
	next := prepareView(t, &a, v.Head)
	if _, err := BuildCommitted(context.Background(), next); err != nil {
		t.Fatalf("the dynamic base does not build: %v", err)
	}
	return next
}

// A delegation or DNSSEC data a dynamic update added hides a client-group
// view just as static data does. The compiler cannot see dynamic records in
// its builder, so it must refuse such a view endpoint itself: committing the
// view would make the committed build of the whole zone fail on every pass,
// and no record set of the zone could commit again (D373, D403).
func TestCompilerRefusesViewsHiddenByDynamicData(t *testing.T) {
	for _, tc := range []struct {
		name, dynamic string
		view          public.Endpoint
		outcome       string
		reason        string
		views         int
	}{
		{"delegation", "sub.example. 60 IN NS ns.other.net.", viewEndpoint("host.sub", "10.0.0.9", "lan"), "duplicated", public.ReasonOccluded, 0},
		{"delegation at the name", "sub.example. 60 IN NS ns.other.net.", viewEndpoint("sub", "10.0.0.9", "lan"), "duplicated", public.ReasonOccluded, 0},
		{"dnskey", "key.example. 60 IN DNSKEY 257 3 13 mdsswUyr3DPW132mOi8V9xESWE8jTo0dxCjjnopKl+GqJxpVXckHAeF+KkxLbxILfDLUT0rAK9iUzy1L53eKGQ==", viewEndpoint("nas", "10.0.0.9", "lan"), "withheld", public.ReasonInvalid, 0},
		{"unrelated dynamic data", "dyn.example. 60 IN A 192.0.2.50", viewEndpoint("nas", "10.0.0.9", "lan"), "served", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := desiredZone()
			c := NewCompiler(CompilerOptions{})
			in := &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{desiredSet("base", endpoint("www", "203.0.113.10"))}, Complete: true}
			dynamic := withDynamic(t, c, z, in, tc.dynamic)
			in.RecordSets = append(in.RecordSets, desiredSet("views", tc.view), desiredSet("unrelated", endpoint("other", "192.0.2.77")))
			result, err := c.Project(context.Background(), dynamic, in, fixedNow.Add(3600e9))
			if err != nil {
				t.Fatal(err)
			}
			outcomes := map[string]zonestate.SourceDiagnostic{}
			for _, d := range result.Diagnostics {
				outcomes[d.SourceID] = d
			}
			if o := outcomes["DNSRecordSet/dns/views"]; o.Outcome != tc.outcome || !strings.HasPrefix(o.Reason, tc.reason) {
				t.Fatalf("view record set %+v, want %s %q", o, tc.outcome, tc.reason)
			}
			if o := outcomes["DNSRecordSet/dns/unrelated"]; o.Outcome != "served" {
				t.Fatalf("unrelated record set %+v", o)
			}
			next := prepareView(t, result.Aggregate, dynamic.Head)
			if len(next.Aggregate.Views) != tc.views {
				t.Fatalf("committed views %+v, want %d", next.Aggregate.Views, tc.views)
			}
			model, err := BuildCommitted(context.Background(), next)
			if err != nil {
				t.Fatalf("the committed build of the zone fails: %v", err)
			}
			if res := model.Lookup("other.example.", dns.TypeA); res.Kind != zone.Success {
				t.Fatalf("the unrelated record set did not commit: %v", res.Kind)
			}
		})
	}
}
