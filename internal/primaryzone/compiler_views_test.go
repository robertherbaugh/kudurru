package primaryzone

import (
	"context"
	"strings"
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

func viewEndpoint(name, target, view string) public.Endpoint {
	e := endpoint(name, target)
	e.Kudurru = &public.EndpointExt{View: view}
	return e
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
