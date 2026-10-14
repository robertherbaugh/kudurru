package blocking

import (
	"errors"
	"io"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestRPZAPIAdapterKeepsExternalRole(t *testing.T) {
	for _, src := range []v1alpha1.ListSource{
		{Format: "rpz", RPZ: &v1alpha1.RPZSource{ZoneName: "policy.test"}},
		{Format: "rpz"}, // Invalid configuration still requires unavailable coverage.
		{URL: "https://invalid.example", RPZ: &v1alpha1.RPZSource{}},
	} {
		spec, err := SpecFromAPI("feed", &v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{src}}, func(int, v1alpha1.ListSource) (func() (io.ReadCloser, error), error) {
			t.Fatal("RPZ passed to text opener")
			return nil, nil
		})
		if err != nil || !spec.ExternalPolicy || len(spec.Sources) != 0 {
			t.Fatalf("adapter: %+v %v", spec, err)
		}
		list := mustCompile(t, spec)
		if list.Stats().Entries != 0 || len(list.Sources()) != 0 {
			t.Fatal("external role invented text rules/statistics")
		}
		snap, err := NewSnapshot(1, list)
		if err != nil {
			t.Fatal(err)
		}
		selection, err := snap.Select([]string{"feed"}, SelectOptions{})
		if err != nil || len(selection.BlockLists()) != 1 || selection.Evaluate("bad.test").Action != ActionPass {
			t.Fatalf("external list lost role: %+v %v", selection, err)
		}
		if SourceName(2, src) != "rpz-2" {
			t.Fatal("RPZ source name disagrees with runtime")
		}
	}
	no := false
	spec, err := SpecFromAPI("disabled", &v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{{Format: "rpz", Enabled: &no}}}, nil)
	if err != nil || spec.ExternalPolicy {
		t.Fatal("disabled RPZ enabled external policy")
	}
	if _, err := Compile(spec, nil, CompileOptions{}); !errors.Is(err, ErrNoSources) {
		t.Fatal("generic empty-list validation changed", err)
	}
}

func TestRPZMixedCompileAndFingerprint(t *testing.T) {
	spec, err := SpecFromAPI("mixed", &v1alpha1.BlockListSpec{Sources: []v1alpha1.ListSource{
		{Format: "rpz", RPZ: &v1alpha1.RPZSource{}},
		{Inline: []string{"||ordinary.test^"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	list := mustCompile(t, spec)
	if _, ok := list.Match("ordinary.test"); !ok || list.Stats().Entries != 1 || len(list.Sources()) != 1 {
		t.Fatal("mixed ordinary source not compiled")
	}
	spec.ExternalPolicy = false
	ordinary := mustCompile(t, spec)
	if ordinary.Fingerprint() == list.Fingerprint() {
		t.Fatal("external policy role missing from fingerprint")
	}
	boom := errors.New("source failure")
	spec.ExternalPolicy = true
	spec.Sources = []Source{{Name: "broken", Format: parser.FormatAuto, Open: func() (io.ReadCloser, error) { return nil, boom }}}
	if list, err := Compile(spec, nil, CompileOptions{}); list != nil || !errors.Is(err, boom) {
		t.Fatal("external role masked ordinary source failure", err)
	}
}

func TestRPZAuthorizationBoundary(t *testing.T) {
	allow := mustCompile(t, Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{"||allowed.test^$client=192.0.2.1,dnstype=A,ctag=device_pc"})}})
	block := mustCompile(t, Spec{Name: "block", Mode: ModeBlock, Sources: []Source{LinesSource("s", parser.FormatAdblock, []string{
		"@@||exception.test^", "||deny.test^", "||rewrite.test^$dnsrewrite=192.0.2.9",
		"||allowed.test^$client=192.0.2.1,dnstype=A,ctag=device_pc,badfilter",
	})}})
	snap, err := NewSnapshot(1, allow, block)
	if err != nil {
		t.Fatal(err)
	}
	ctx := Context{Name: "allowed.test.", Type: dns.TypeA, Class: dns.ClassINET, ClientIP: netip.MustParseAddr("192.0.2.1"), Tags: []string{"device_pc"}}
	sel, err := snap.Select([]string{"allow", "block"}, SelectOptions{ExceptionScope: ScopeSelection})
	if err != nil {
		t.Fatal(err)
	}
	if d := sel.EvaluateAuthorizationContext(ctx); d.Action != ActionAllow || d.Reason != ReasonAllowList {
		t.Fatal("block badfilter canceled explicit allow", d)
	}
	for _, name := range []string{"exception.test", "deny.test", "rewrite.test"} {
		ctx.Name = name
		if d := sel.EvaluateAuthorizationContext(ctx); d.Action != ActionPass {
			t.Fatalf("ordinary policy ran before RPZ: %+v", d)
		}
	}
	ctx.Name, ctx.Type = "allowed.test", dns.TypeAAAA
	if d := sel.EvaluateAuthorizationContext(ctx); d.Action != ActionPass {
		t.Fatal("allow selector bypassed", d)
	}
	ctx.Type = dns.TypeA
	isolated, err := snap.Select([]string{"block"}, SelectOptions{AllowOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := isolated.EvaluateAuthorizationContext(ctx); d.Action != ActionBlock || d.Reason != ReasonAllowOnly {
		t.Fatal("unselected allow-list granted authorization", d)
	}
	var absent *Selection
	if absent.EvaluateAuthorizationContext(ctx).Action != ActionPass {
		t.Fatal("nil authorization selection")
	}
}
