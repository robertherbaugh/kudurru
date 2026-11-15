package primaryzone

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCompilerCommitsEveryPairOfEndpoints closes the class of D373 against
// the committed builder rather than the desired one: every pair of
// endpoints over a small alphabet of names, types, set identifiers and
// targets, in one record set or split across two, must project, prepare and
// pass the runtime's own prebuild (zonestate.Materializer with
// BuildCommitted, as primaryruntime does before admission), without a
// committed head (the first start after an upgrade) and next to one (a
// running node). A shape the desired builder takes but
// BuildCommittedPrimary refuses (an invalid CNAME or DNAME RRset, a CNAME
// next to data, data below a DNAME, data ValidateData refuses) would stop
// the zone; this test fails on any such shape.
func TestCompilerCommitsEveryPairOfEndpoints(t *testing.T) {
	type shape struct{ name, typ, target, setID string }
	targets := map[string][2]string{
		"A":     {"192.0.2.1", "192.0.2.2"},
		"TXT":   {"one", "two"},
		"CNAME": {"t1.test.", "t2.test."},
		"DNAME": {"t1.test.", "t2.test."},
		"NS":    {"ns1.test.", "ns2.test."},
	}
	var shapes []shape
	for _, name := range []string{"example.", "a.example.", "x.a.example."} {
		for _, typ := range []string{"A", "TXT", "CNAME", "DNAME", "NS"} {
			for _, setID := range []string{"", "s"} {
				for _, target := range targets[typ] {
					shapes = append(shapes, shape{name, typ, target, setID})
				}
			}
		}
	}
	ep := func(s shape) public.Endpoint {
		return public.Endpoint{DNSName: s.name, RecordType: s.typ, Targets: []string{s.target}, SetIdentifier: s.setID}
	}
	z := desiredZone()
	ok := desiredSet("ok", endpoint("www", "192.0.2.10"))
	ok.CreationTimestamp = metav1.NewTime(fixedNow.Add(-time.Hour))
	c := NewCompiler(CompilerOptions{})
	head := compile(t, c, blank(z), &zonestate.DesiredInputs{Zone: z, RecordSets: []*public.DNSRecordSet{ok}, Complete: true}, fixedNow)
	check := func(base *zonestate.View, sets []*public.DNSRecordSet) error {
		in := &zonestate.DesiredInputs{Zone: z, RecordSets: append([]*public.DNSRecordSet{ok}, sets...), Complete: true}
		result, err := NewCompiler(CompilerOptions{}).Project(context.Background(), base, in, fixedNow.Add(time.Second))
		if err != nil {
			return fmt.Errorf("project: %w", err)
		}
		p, err := zonestate.Prepare(result.Aggregate, base.Head, zonestate.Limits{})
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		m, _ := zonestate.NewMaterializer(zonestate.MaterializerOptions{Build: BuildCommitted})
		candidate := &zonestate.View{Head: p.Head, Aggregate: result.Aggregate}
		if _, err := m.Prepare(context.Background(), candidate); err != nil {
			return fmt.Errorf("runtime prebuild: %w", err)
		}
		model, err := BuildCommitted(context.Background(), candidate)
		if err != nil {
			return fmt.Errorf("build: %w", err)
		}
		if a := model.Lookup("www.example.", dns.TypeA).Answer; len(a) != 1 {
			return fmt.Errorf("the rest of the zone is not served: %v", a)
		}
		for _, d := range result.Diagnostics {
			if d.Outcome != "served" && d.Reason == "" {
				return fmt.Errorf("%s %s without a reason", d.SourceID, d.Outcome)
			}
		}
		return nil
	}
	var failures, runs int
	for _, first := range shapes {
		// The second target of a type only matters next to the first.
		if first.target != targets[first.typ][0] {
			continue
		}
		for _, second := range shapes {
			if second == first {
				continue
			}
			one := desiredSet("one", ep(first), ep(second))
			split1 := desiredSet("first", ep(first))
			split2 := desiredSet("second", ep(second))
			split2.CreationTimestamp = metav1.NewTime(fixedNow.Add(time.Minute))
			for _, run := range []struct {
				base *zonestate.View
				sets []*public.DNSRecordSet
			}{{blank(z), []*public.DNSRecordSet{one}}, {blank(z), []*public.DNSRecordSet{split1, split2}}, {head, []*public.DNSRecordSet{split1, split2}}} {
				runs++
				if err := check(run.base, run.sets); err != nil {
					failures++
					if failures <= 20 {
						t.Errorf("%+v then %+v (%d record sets, head %t): %v", first, second, len(run.sets), run.base.Head != nil, err)
					}
				}
			}
		}
	}
	if failures > 0 {
		t.Fatalf("%d of %d combinations stop the zone", failures, runs)
	}
}
