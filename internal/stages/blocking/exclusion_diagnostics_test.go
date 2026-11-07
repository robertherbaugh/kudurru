package blocking

import (
	"testing"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

func TestQuestionExclusionsSelectedPolicyAndOriginalRequest(t *testing.T) {
	build := func(exclude bool) *policy.Index {
		t.Helper()
		src := engine.LinesSource("source", parser.FormatDomains, []string{"raw.example", "target.example"})
		if exclude {
			src.Exclude = []string{"raw.example", "target.example"}
		}
		list, err := engine.Compile(engine.Spec{Name: "default/rules", Mode: engine.ModeBlock, Sources: []engine.Source{src}}, nil, engine.CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap, err := engine.NewSnapshot(1, list)
		if err != nil {
			t.Fatal(err)
		}
		return policy.Build(policy.Input{ClientGroups: []*public.ClientGroup{cg("selected", public.ClientMatcher{CIDR: "192.0.2.0/24"}), cg("other", public.ClientMatcher{CIDR: "198.51.100.0/24"})}, Policies: []*public.Policy{{ObjectMeta: meta("selected"), Spec: public.PolicySpec{ClientGroupRefs: []string{"selected"}, BlockListRefs: []string{"rules"}}}}, BlockLists: []*public.BlockList{bl("rules", "block", inlineSource)}, Lists: snap}, policy.Options{})
	}
	old, fresh := build(true), build(false)
	stage, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	terminal := &modifierTerminal{}
	for _, test := range []struct {
		name, client      string
		index             *policy.Index
		collect, disabled bool
		want              int
	}{
		{"diagnostic", "192.0.2.1", old, true, false, 1},
		{"ordinary traffic", "192.0.2.1", old, false, false, 0},
		{"unselected client", "198.51.100.1", old, true, false, 0},
		{"new publication", "192.0.2.1", fresh, true, false, 0},
		{"held old publication", "192.0.2.1", old, true, false, 1},
		{"disabled filtering", "192.0.2.1", old, true, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			stage.Reconfigure(Options{Disabled: test.disabled})
			req := modifierRequest(test.index, "raw.example.", dns.TypeA, test.client)
			req.CollectExclusions = test.collect
			out, err := stage.Serve(req, func(r *pipeline.Request) (*pipeline.Response, error) {
				// A synchronous target gets a copy of the request. Its evidence
				// must not overwrite the root question's exclusions.
				sub := *r
				sub.Msg = query("target.example.", dns.TypeA, false)
				child, err := stage.Serve(&sub, func(target *pipeline.Request) (*pipeline.Response, error) { return terminal.Serve(target, nil) })
				if child != nil {
					child.Close()
				}
				if err != nil {
					return nil, err
				}
				return terminal.Serve(r, nil)
			})
			if out != nil {
				out.Close()
			}
			if err != nil || req.ExclusionsTruncated || len(req.Exclusions) != test.want {
				t.Fatal("selected diagnostic evidence changed", req.Exclusions, err)
			}
			if test.want != 0 && req.Exclusions[0].Rule != "raw.example" {
				t.Fatal("subquery replaced root evidence")
			}
		})
	}
}
