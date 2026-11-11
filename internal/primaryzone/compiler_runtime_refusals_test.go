package primaryzone

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runtimeCheck runs what the primary runtime runs on a compiled candidate
// before admission (primaryruntime.Reconcile): zonestate.Prepare with the
// zone limits, then the Materializer's prebuild with BuildCommitted. It
// returns the projection of every record set.
func runtimeCheck(base *zonestate.View, z *public.DNSZone, sets []*public.DNSRecordSet) (map[string]zonestate.SourceDiagnostic, *zonestate.View, error) {
	in := &zonestate.DesiredInputs{Zone: z, RecordSets: sets, Complete: true}
	result, err := NewCompiler(CompilerOptions{}).Project(context.Background(), base, in, fixedNow.Add(time.Second))
	if err != nil {
		return nil, nil, fmt.Errorf("project: %w", err)
	}
	p, err := zonestate.Prepare(result.Aggregate, base.Head, zonestate.Limits{})
	if err != nil {
		return nil, nil, fmt.Errorf("zonestate.Prepare: %w", err)
	}
	candidate := &zonestate.View{Head: p.Head, Aggregate: result.Aggregate}
	m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
	if _, err := m.Prepare(context.Background(), candidate); err != nil {
		return nil, nil, fmt.Errorf("runtime prebuild: %w", err)
	}
	out := map[string]zonestate.SourceDiagnostic{}
	for _, d := range result.Diagnostics {
		out[d.SourceID] = d
	}
	return out, candidate, nil
}

// TestCompilerWithholdsEveryRefusalOfThePrimaryRuntime is the table of
// D373 for the primary runtime: every place zonestate (normalized,
// ReplaceStatic) and the committed builder (BuildCommittedPrimary, the
// runtime's prebuild) refuse a candidate, how the compiler keeps a record
// set from reaching it, and a record set shape that would reach it. Each
// shape is compiled without a committed head (the first start after an
// upgrade) and next to one (a running node, with a record a dynamic update
// added), and must leave the rest of the zone committed and served while
// the shape itself is left out and reported. TestCompilerWithholdsEveryRefusalOfTheBuilder
// is the table for the desired builder; TestCompilerCommitsEveryPairOfEndpoints
// covers pairs of endpoints. Zone-wide capacity bounds are not a record
// set shape and are listed at the end of the table without a case.
func TestCompilerWithholdsEveryRefusalOfThePrimaryRuntime(t *testing.T) {
	typed := func(name, typ string, targets ...string) public.Endpoint {
		return public.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
	}
	setID := func(ep public.Endpoint, id string) public.Endpoint { ep.SetIdentifier = id; return ep }
	type row struct {
		site, handling string
		// sets are the record sets of the shape, after "ok" (www A).
		sets [][]public.Endpoint
		// dynamicOnly: the shape needs a record a dynamic update added,
		// so it only runs next to a committed head.
		dynamicOnly bool
		// gone is a name and type that must not be served when all the
		// record sets of the shape are written.
		gone  string
		gtype uint16
	}
	rows := []row{
		{"zonestate.normalized: RDATA (ValidateData), and BuildCommittedPrimary ValidateData", "committable (zonestate.FromRR) before the builder takes the endpoint",
			[][]public.Endpoint{{typed("empty-rdata.example.", "A", `\# 0`)}}, false, "empty-rdata.example.", dns.TypeA},
		{"zonestate.normalized: record framing, uncompressed record required", "committable: the record is stored in the codec's own wire form (dynamicupdate.Wire)",
			[][]public.Endpoint{{typed("raw.example.", "A", `\# 4 c0000201`)}}, false, "", 0},
		{"zonestate.normalized: owner/class/type; BuildCommittedPrimary: invalid owner/class/type", "the builder parses class IN, in-zone owners and no meta type (sweep below); outside-zone owners are withheld",
			[][]public.Endpoint{{typed("opt.example.", "TYPE41", `\# 0`)}, {typed("tsig.example.", "TYPE250", `\# 0`)}, {typed("any.example.", "TYPE255", `\# 0`)}, {typed("reserved.example.", "TYPE65535", `\# 0`)}, {typed("www.elsewhere.", "A", "192.0.2.60")}}, false, "opt.example.", 41},
		{"zonestate.normalized: duplicate record; BuildCommittedPrimary: duplicate/invalid RR", "the builder keeps the oldest record set of an identity, a later one is duplicated",
			[][]public.Endpoint{{typed("twin.example.", "A", "192.0.2.61")}, {typed("twin.example.", "A", "192.0.2.61")}}, false, "", 0},
		{"zonestate.normalized: apex SOA/NS required; BuildCommittedPrimary: apex SOA and configured NS", "the builder generates apex SOA and NS; apex SOA, NS and CNAME endpoints are withheld",
			[][]public.Endpoint{{typed("example.", "SOA", "ns.example. host.example. 1 2 3 4 5")}, {typed("example.", "NS", "other.test.")}, {typed("example.", "CNAME", "other.test.")}}, false, "", 0},
		{"BuildCommittedPrimary: one apex SOA with committed serial required", "the builder generates the one SOA; an SOA endpoint below the apex is refused",
			[][]public.Endpoint{{typed("deep.example.", "SOA", "ns.example. host.example. 1 2 3 4 5")}}, false, "deep.example.", dns.TypeSOA},
		{"BuildCommittedPrimary: invalid CNAME RRset", "the builder keeps one CNAME target at a name, across set identifiers and record sets",
			[][]public.Endpoint{{setID(typed("alias.example.", "CNAME", "a.test."), "x"), setID(typed("alias.example.", "CNAME", "b.test."), "y")}, {typed("alias.example.", "CNAME", "c.test.")}}, false, "", 0},
		{"BuildCommittedPrimary: invalid DNAME RRset", "the builder keeps one DNAME target at a name, across set identifiers and record sets",
			[][]public.Endpoint{{setID(typed("tree.example.", "DNAME", "a.test."), "x"), setID(typed("tree.example.", "DNAME", "b.test."), "y")}, {typed("tree.example.", "DNAME", "c.test.")}}, false, "", 0},
		{"BuildCommittedPrimary: CNAME coexists with data", "the builder refuses a CNAME next to data and data next to a CNAME, in either order",
			[][]public.Endpoint{{typed("both.example.", "A", "192.0.2.62")}, {typed("both.example.", "CNAME", "a.test.")}, {typed("both2.example.", "CNAME", "a.test."), typed("both2.example.", "TXT", "x")}}, false, "", 0},
		{"BuildCommittedPrimary: data below DNAME", "the builder reports data below a DNAME owner as occluded",
			[][]public.Endpoint{{typed("branch.example.", "DNAME", "a.test.")}, {typed("leaf.branch.example.", "A", "192.0.2.63")}}, false, "leaf.branch.example.", dns.TypeA},
		{"zonestate.ReplaceStatic: static projection conflicts with dynamic ownership", "StaticConflicts names the record set; the compiler leaves it out (duplicated) and compiles the rest",
			[][]public.Endpoint{{typed("dyn.example.", "A", "192.0.2.64")}, {typed("dyn.example.", "CNAME", "a.test.")}, {typed("example.", "DNAME", "a.test.")}, {typed("dyn.example.", "DNAME", "a.test.")}}, true, "", 0},
		// Zone-wide bounds, not record set shapes, and open: a zone over
		// one of them is refused as a whole (ErrCapacity) and keeps its
		// last commit. zonestate.Limits: MaxRecords 1,000,000,
		// MaxWireBytes 64 MiB, MaxAggregateBytes 128 MiB, MaxChunks 1024;
		// BuildCommittedPrimary: 1,000,000 records and 64 MiB; the
		// compiler: the source objects within MaxAggregateBytes.
		{"capacity: zonestate.Limits and BuildCommittedPrimary bounds", "open: zone-wide, not left out per record set", nil, false, "", 0},
	}
	z := desiredZone()
	ok := desiredSet("ok", endpoint("www", "192.0.2.10"))
	ok.CreationTimestamp = metav1.NewTime(fixedNow.Add(-time.Hour))
	c := NewCompiler(CompilerOptions{})
	head := compile(t, c, blank(z), &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{ok}, Complete: true}, fixedNow)
	// A record a dynamic update added at dyn.example. next to the head.
	dynamic := func() *zonestate.View {
		rr, _ := dns.NewRR("dyn.example. 60 IN A 192.0.2.9")
		r, _ := zonestate.FromRR(rr, zonestate.Provenance{Key: "key.", CredentialVersion: "v1"})
		a := *head.Aggregate
		a.Records = append(append([]zonestate.Record(nil), a.Records...), r)
		for i, r := range a.Records {
			rr, _, _ := dns.UnpackRR(r.Wire, 0)
			if soa, ok := rr.(*dns.SOA); ok {
				soa.Serial++
				a.Records[i], _ = zonestate.FromRR(soa, r.Provenance)
			}
		}
		return prepareView(t, &a, head.Head)
	}()
	for _, r := range rows {
		if r.sets == nil {
			t.Logf("%s: %s (no case)", r.site, r.handling)
			continue
		}
		bases := map[string]*zonestate.View{"no head": blank(z), "head": head, "head with a dynamic record": dynamic}
		if r.dynamicOnly {
			bases = map[string]*zonestate.View{"head with a dynamic record": dynamic}
		}
		// Each record set alone, and all of them together.
		var shapes [][]*public.DNSRecordSet
		var all []*public.DNSRecordSet
		for i, eps := range r.sets {
			rs := desiredSet(fmt.Sprintf("shape%d", i), eps...)
			rs.CreationTimestamp = metav1.NewTime(fixedNow.Add(time.Duration(i) * time.Minute))
			shapes = append(shapes, []*public.DNSRecordSet{rs})
			all = append(all, rs)
		}
		if len(all) > 1 {
			shapes = append(shapes, all)
		}
		for label, base := range bases {
			for _, sets := range shapes {
				diagnostics, candidate, err := runtimeCheck(base, z, append([]*public.DNSRecordSet{ok}, sets...))
				if err != nil {
					t.Errorf("%s (%s, %d record sets): the zone is stopped: %v", r.site, label, len(sets), err)
					continue
				}
				model, err := BuildCommitted(context.Background(), candidate)
				if err != nil {
					t.Errorf("%s (%s): build: %v", r.site, label, err)
					continue
				}
				if a := model.Lookup("www.example.", dns.TypeA).Answer; len(a) != 1 {
					t.Errorf("%s (%s): the rest of the zone is not served: %v", r.site, label, a)
				}
				if r.gone != "" && len(sets) == len(all) {
					if a := model.Lookup(r.gone, r.gtype).Answer; len(a) != 0 && a[0].Header().Rrtype == r.gtype && dns.CanonicalName(a[0].Header().Name) == r.gone {
						t.Errorf("%s (%s): %s %s is served: %v", r.site, label, r.gone, dns.TypeToString[r.gtype], a)
					}
				}
				if base == dynamic {
					if a := model.Lookup("dyn.example.", dns.TypeA).Answer; len(a) != 1 || a[0].(*dns.A).A.String() != "192.0.2.9" {
						t.Errorf("%s (%s): the dynamic record is not kept: %v", r.site, label, a)
					}
				}
				for _, rs := range sets {
					d := diagnostics[store.KeyOf(rs).String()]
					if d.Outcome != "served" && d.Reason == "" {
						t.Errorf("%s (%s): %s is %s without a reason", r.site, label, d.SourceID, d.Outcome)
					}
				}
			}
		}
	}
	if r := rows[len(rows)-2]; !strings.HasPrefix(r.site, "zonestate.ReplaceStatic") {
		t.Fatal("the table lost its dynamic ownership row")
	}

	// The sweep behind the owner/class/type row: every record type the
	// DNS library knows, and the meta and reserved numbers, with targets of
	// many shapes. Whatever the compiler projects passes the runtime.
	t.Run("every record type", func(t *testing.T) {
		targets := []string{"x", "1", "192.0.2.1", "2001:db8::1", `\# 0`, `\# 4 c0000201`, "10 mx.example.", "0 0 0 x.example.", "ns.example.", "1 1 1 00", `0 issue "ca"`, "256 3 8 AwEAAa=="}
		var types []string
		for name := range dns.StringToType {
			types = append(types, name)
		}
		types = append(types, "TYPE0", "TYPE41", "TYPE250", "TYPE255", "TYPE65280", "TYPE65535")
		sort.Strings(types)
		served := 0
		for _, typ := range types {
			for _, target := range targets {
				for _, name := range []string{"p.example.", "example."} {
					rs := desiredSet("probe", public.Endpoint{DNSName: name, RecordType: typ, Targets: []string{target}})
					diagnostics, _, err := runtimeCheck(blank(z), z, []*public.DNSRecordSet{ok, rs})
					if err != nil {
						t.Errorf("%s %s %q: %v", name, typ, target, err)
						continue
					}
					if diagnostics[store.KeyOf(rs).String()].Outcome == "served" {
						served++
					}
				}
			}
		}
		// The sweep must reach the runtime with real records, not only
		// withhold them all.
		if served < 50 {
			t.Fatalf("only %d probes were served", served)
		}
	})
}
