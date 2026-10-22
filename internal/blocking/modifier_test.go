package blocking

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func selectionFor(t testing.TB, lines ...string) *Selection {
	t.Helper()
	l, e := Compile(Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("inline", parser.FormatAdblock, lines)}}, nil, CompileOptions{})
	if e != nil {
		t.Fatal(e)
	}
	s, e := NewSnapshot(1, l)
	if e != nil {
		t.Fatal(e)
	}
	sel, e := s.Select([]string{"rules"}, SelectOptions{})
	if e != nil {
		t.Fatal(e)
	}
	return sel
}
func TestModifierContextConditions(t *testing.T) {
	base := Context{Name: "ads.example.", Type: dns.TypeA, Class: dns.ClassINET, ClientIP: netip.MustParseAddr("192.0.2.5"), ClientID: "device-id", ClientNames: []string{"Frank's laptop", "with,comma|pipe"}, Tags: []string{"device_pc", "user_child"}}
	for _, tc := range []struct {
		modifier string
		want     bool
	}{
		{"dnstype=A", true}, {"dnstype=AAAA", false}, {"dnstype=~AAAA|~TXT", true}, {"dnstype=~A|~CNAME", false}, {"dnstype=~A|AAAA", false}, {"dnstype=~AAAA|A", true},
		{"client=192.0.2.5", true}, {"client=192.0.2.0/24", true}, {"client=~192.0.2.5", false}, {"client=192.0.2.0/24|~192.0.2.5", false}, {"client='Frank\\'s laptop'", true}, {"client='with\\,comma\\|pipe'", true}, {"client=elsewhere", false}, {"client=device-id", true},
		{"ctag=device_pc", true}, {"ctag=os_ios", false}, {"ctag=~user_admin", true}, {"ctag=user_child|~device_pc", false}, {"denyallow=example", false}, {"denyallow=good.ads.example", true}, {"dnstype=A,ctag=device_pc,client=192.0.2.0/24", true},
	} {
		t.Run(tc.modifier, func(t *testing.T) {
			sel := selectionFor(t, "||ads.example^$"+tc.modifier)
			d := sel.EvaluateContext(base)
			if d.Blocked() != tc.want {
				t.Fatalf("decision %+v", d)
			}
			if tc.want && d.Rule() != "||ads.example^$"+tc.modifier {
				t.Fatal("modifier provenance lost")
			}
		})
	}
}
func TestCompleteSlowRuleStreamAndImmutability(t *testing.T) {
	var lines []string
	for i := 0; i < 2500; i++ {
		lines = append(lines, fmt.Sprintf("||host%d.example^$dnstype=AAAA", i))
	}
	lines = append(lines, "||last.example^$client=192.0.2.0/24,important")
	l, e := Compile(Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("first", parser.FormatAdblock, lines[:1500]), LinesSource("last", parser.FormatAdblock, lines[1500:])}}, nil, CompileOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if l.Stats().Entries != 2501 || l.Stats().Unsupported != 0 {
		t.Fatalf("stream truncated: %+v", l.Stats())
	}
	s, _ := NewSnapshot(1, l)
	sel, _ := s.Select([]string{"rules"}, SelectOptions{})
	for _, name := range []string{"host0.example", "host1001.example", "host2499.example"} {
		if !sel.EvaluateContext(Context{Name: name, Type: dns.TypeAAAA}).Blocked() {
			t.Fatal("rule missing " + name)
		}
	}
	if !sel.EvaluateContext(Context{Name: "last.example", Type: dns.TypeA, ClientIP: netip.MustParseAddr("192.0.2.8")}).Blocked() {
		t.Fatal("last source rule lost")
	}
	lines[0] = "changed"
	if !sel.EvaluateContext(Context{Name: "host0.example", Type: dns.TypeAAAA}).Blocked() {
		t.Fatal("input mutation changed compiled rule")
	}
}
func TestBadfilterCompleteListAndScope(t *testing.T) {
	for _, lines := range [][]string{
		{"||ads.example^", "||ads.example^$badfilter"},
		{"||ads.example^$badfilter", "||ads.example^"},
		{"||ads.example^$dnstype=AAAA,client=192.0.2.5", "||ads.example^$client=192.0.2.5,dnstype=AAAA,badfilter"},
		{"@@||ads.example^", "@@||ads.example^$badfilter", "||ads.example^"},
		{"||ADS.example|", "||ads.example|$badfilter"},
		{"*.ads.example^", "*.ads.example^$badfilter"},
	} {
		sel := selectionFor(t, lines...)
		d := sel.EvaluateContext(Context{Name: "ads.example", Type: dns.TypeAAAA, ClientIP: netip.MustParseAddr("192.0.2.5")})
		want := strings.HasPrefix(lines[0], "@@")
		if d.Blocked() != want {
			t.Fatalf("badfilter %v: %+v", lines, d)
		}
	}
	target, _ := Compile(Spec{Name: "target", Mode: ModeBlock, Sources: []Source{LinesSource("targets", parser.FormatAdblock, []string{"||example^", "||ads.example^", "/^ads\\.example$/"})}}, nil, CompileOptions{})
	cancel, _ := Compile(Spec{Name: "cancel", Mode: ModeBlock, Sources: []Source{LinesSource("cancel", parser.FormatAdblock, []string{"||example^$badfilter", "||ads.example^$badfilter", "/^ads\\.example$/$badfilter"})}}, nil, CompileOptions{})
	s, _ := NewSnapshot(1, target, cancel)
	local, _ := s.Select([]string{"target", "cancel"}, SelectOptions{})
	if !local.Evaluate("ads.example").Blocked() {
		t.Fatal("local badfilter cancelled another list")
	}
	merged, _ := s.Select([]string{"target", "cancel"}, SelectOptions{ExceptionScope: ScopeSelection})
	if merged.Evaluate("ads.example").Blocked() {
		t.Fatal("merged badfilter failed to cancel alternate matchers")
	}
	again, _ := s.Select([]string{"target"}, SelectOptions{})
	if !again.Evaluate("ads.example").Blocked() {
		t.Fatal("selection mutated shared list")
	}
}
func TestImportantExceptionPrecedenceAndExplicitAllow(t *testing.T) {
	for _, tc := range []struct {
		rules   []string
		blocked bool
	}{
		{[]string{"||ads.example^$important", "@@||ads.example^"}, true},
		{[]string{"||ads.example^$important", "@@||ads.example^$important"}, false},
		{[]string{"||ads.example^", "@@||ads.example^$dnstype=A"}, false},
		{[]string{"||ads.example^$important,client=192.0.2.5", "@@||ads.example^$important,ctag=device_pc"}, false},
	} {
		sel := selectionFor(t, tc.rules...)
		if d := sel.EvaluateContext(Context{Name: "ads.example", Type: dns.TypeA, ClientIP: netip.MustParseAddr("192.0.2.5"), Tags: []string{"device_pc"}}); d.Blocked() != tc.blocked {
			t.Fatalf("%v: %+v", tc.rules, d)
		}
	}
	deny := selectionFor(t, "||ads.example^$important").block[0]
	allow, _ := Compile(Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"||ads.example^$ctag=device_pc"})}}, nil, CompileOptions{})
	s, _ := NewSnapshot(1, deny, allow)
	sel, _ := s.Select([]string{"rules", "allow"}, SelectOptions{})
	d := sel.EvaluateContext(Context{Name: "ads.example", Type: dns.TypeA, Tags: []string{"device_pc"}})
	if d.Reason != ReasonAllowList {
		t.Fatal("explicit selected allow list lost")
	}
}
func TestInvalidAndUnknownModifiersNeverBroadenRule(t *testing.T) {
	lines := []string{"||bad.example^$dnstype=NONE", "||bad.example^$dnstype=", "||bad.example^$client=192.0.2.0/999", "||bad.example^$client=~", "||bad.example^$ctag=imaginary", "||bad.example^$denyallow=", "||bad.example^$important=yes", "||bad.example^$important,important", "||bad.example^$dnsrewrite=NOERROR;A;not-an-ip", "||bad.example^$script", "||bad.example^$dnstype=A,unknown=true", "||bad.example^$respgeo=US"}
	sel := selectionFor(t, lines...)
	l := sel.block[0]
	if l.Stats().Invalid != 11 || l.Stats().Unsupported != 1 || l.Stats().Entries != 0 {
		t.Fatalf("accounting %+v", l.Stats())
	}
	if sel.Evaluate("bad.example").Blocked() {
		t.Fatal("invalid condition became unconditional block")
	}
}
func TestTypedRewritesAndExceptions(t *testing.T) {
	for _, tc := range []struct {
		value string
		typ   uint16
		count int
		rcode int
	}{
		{"192.0.2.1", dns.TypeA, 1, 0}, {"2001:db8::1", dns.TypeAAAA, 1, 0}, {"192.0.2.1", dns.TypeAAAA, 0, 0}, {"target.example", dns.TypeA, 1, 0}, {"NXDOMAIN", dns.TypeA, 0, dns.RcodeNameError}, {"REFUSED;;", dns.TypeA, 0, dns.RcodeRefused}, {"NOERROR;;", dns.TypeA, 0, 0},
		{"NOERROR;PTR;target.example.", dns.TypePTR, 1, 0}, {"NOERROR;TXT;hello_world", dns.TypeTXT, 1, 0}, {"NOERROR;MX;32 mail.example.", dns.TypeMX, 1, 0}, {"NOERROR;SRV;10 60 8080 target.example.", dns.TypeSRV, 1, 0}, {"NOERROR;HTTPS;32 target.example. alpn=h3", dns.TypeHTTPS, 1, 0}, {"NOERROR;SVCB;32 target.example. alpn=h3", dns.TypeSVCB, 1, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			sel := selectionFor(t, "||rewrite.example^$dnsrewrite="+tc.value, "||rewrite.example^$important")
			d := sel.EvaluateContext(Context{Name: "rewrite.example", Type: tc.typ})
			if d.Action != ActionRewrite || d.Rewrite.Rcode() != tc.rcode {
				t.Fatalf("rewrite %+v", d)
			}
			rrs := d.Rewrite.Records("rewrite.example.", tc.typ, 1, 60)
			if len(rrs) != tc.count {
				t.Fatalf("records %v", rrs)
			}
			if len(rrs) > 0 {
				rrs[0].Header().Name = "changed."
				if d.Rewrite.Records("rewrite.example.", tc.typ, 1, 60)[0].Header().Name != "rewrite.example." {
					t.Fatal("shared RR mutated")
				}
			}
		})
	}
	sel := selectionFor(t, "||rewrite.example^$dnsrewrite=192.0.2.1", "||rewrite.example^$dnsrewrite=192.0.2.2", "@@||rewrite.example^$dnsrewrite=192.0.2.1")
	d := sel.EvaluateContext(Context{Name: "rewrite.example", Type: dns.TypeA})
	rrs := d.Rewrite.Records("rewrite.example.", 1, 1, 60)
	if len(rrs) != 1 || rrs[0].(*dns.A).A.To4()[3] != 2 {
		t.Fatalf("typed exception %v", rrs)
	}
	all := selectionFor(t, "||rewrite.example^$dnsrewrite=192.0.2.1", "@@||rewrite.example^$dnsrewrite")
	if d := all.Evaluate("rewrite.example"); d.Action == ActionRewrite || d.Blocked() {
		t.Fatal("rewrite exception became deny")
	}
}
func TestResponseSelectorsRetainClientAndQuestionType(t *testing.T) {
	sel := selectionFor(t, "||target.example^$client=192.0.2.0/24,dnstype=AAAA", "||192.0.2.66^$ctag=device_pc")
	ctx := Context{Type: dns.TypeA, ClientIP: netip.MustParseAddr("192.0.2.5")}
	if d := sel.EvaluateResponseContext(ctx, []string{"target.example"}, nil); d.Blocked() {
		t.Fatal("response lost question dnstype")
	}
	ctx.Type = dns.TypeAAAA
	if d := sel.EvaluateResponseContext(ctx, []string{"target.example"}, nil); !d.Blocked() || !d.Response {
		t.Fatal("matching response not blocked")
	}
	ctx.Tags = []string{"device_pc"}
	if d := sel.EvaluateResponseContext(ctx, nil, []netip.Addr{netip.MustParseAddr("192.0.2.66")}); !d.Blocked() {
		t.Fatal("conditional IP response rule ignored")
	}
}

func TestBadfilterPreservesUncanceledEquivalentFastSpellings(t *testing.T) {
	for _, entries := range [][]string{
		{"||ads.example^", "*.ads.example"},
		{"*.ads.example", "||ads.example^"},
		{"||example^", "*.example"},
		{"|ads.example^", "0.0.0.0 ads.example"},
	} {
		target, err := Compile(Spec{Name: "target", Mode: ModeBlock, Sources: []Source{LinesSource("target", parser.FormatAdblock, entries)}}, nil, CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cancel, err := Compile(Spec{Name: "cancel", Mode: ModeBlock, Sources: []Source{LinesSource("cancel", parser.FormatAdblock, []string{entries[0] + "$badfilter"})}}, nil, CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap, _ := NewSnapshot(1, target, cancel)
		sel, _ := snap.Select([]string{"target", "cancel"}, SelectOptions{ExceptionScope: ScopeSelection})
		if !sel.Evaluate("ads.example").Blocked() {
			t.Fatalf("alternate spelling lost for %v", entries)
		}
	}
	// A hosts entry remains independent even if its rendered domain is the
	// same as a canceled plain-domain entry, in either addition order.
	for _, lines := range [][]string{
		{"ads.example", "0.0.0.0 ads.example", "ads.example$badfilter"},
		{"0.0.0.0 ads.example", "ads.example", "ads.example$badfilter"},
	} {
		if !selectionFor(t, lines...).Evaluate("ads.example").Blocked() {
			t.Fatal("badfilter edited a hosts entry")
		}
	}
}

func TestSlowRuleNormalizationAndDeterministicTies(t *testing.T) {
	for _, rule := range []string{"||BÜCHER.Example.^$important", "|BÜCHER.Example.^$important", "*.BÜCHER.Example.$important"} {
		if !selectionFor(t, rule).Evaluate("xn--bcher-kva.example.").Blocked() {
			t.Fatalf("IDNA rule failed: %s", rule)
		}
	}
	// Both equal-specificity rules match. Query gram order must not steal
	// provenance from an earlier rule stored in another indexed bucket.
	first := "alpha*bravo$important"
	sel := selectionFor(t, first, "bravo*alpha$important")
	if d := sel.Evaluate("bravo.alpha.bravo"); d.Rule() != first {
		t.Fatalf("tie changed source order: %+v", d)
	}
}

func TestModifierRegexReloadCapsExclusionsAndDiagnostics(t *testing.T) {
	spec := Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("one", parser.FormatAdblock, []string{
		"/^old\\.example$/$dnstype=A", "/^new\\.example$/$dnstype=AAAA", "/^dropped\\.example$/$important", "||cancel.example^", "||cancel.example^$badfilter", "||excluded.example^$important",
	})}}
	spec.Sources[0].Exclude = []string{"||cancel.example^$badfilter", "||excluded.example^$important"}
	l, err := Compile(spec, nil, CompileOptions{RegexHardCap: 2})
	if err != nil {
		t.Fatal(err)
	}
	if st := l.Stats(); st.Regexes != 2 || st.RegexDropped != 1 || len(l.Unsupported()) != 0 {
		t.Fatalf("stats %+v", st)
	}
	if l.Sources()[0].Excluded != 2 {
		t.Fatal("slow exclusions not counted")
	}
	snap, _ := NewSnapshot(1, l)
	sel, _ := snap.Select([]string{"rules"}, SelectOptions{})
	if !sel.Evaluate("old.example").Blocked() || !sel.Evaluate("cancel.example").Blocked() || sel.Evaluate("excluded.example").Blocked() {
		t.Fatal("cap/exclusion semantics")
	}
	changed := spec
	changed.Sources = []Source{LinesSource("one", parser.FormatAdblock, []string{"/^new\\.example$/$dnstype=A"})}
	next, reports := Rebuild(snap, []Spec{changed}, CompileOptions{})
	if reports[0].Reused || reports[0].Err != nil {
		t.Fatalf("reload %+v", reports)
	}
	fresh, _ := next.Select([]string{"rules"}, SelectOptions{})
	if fresh.Evaluate("old.example").Blocked() || !fresh.Evaluate("new.example").Blocked() || !sel.Evaluate("old.example").Blocked() {
		t.Fatal("regex invalidation mutated or reused old matcher")
	}
}

func TestRewritePrioritiesAndContextDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		rules         []string
		code, records int
	}{
		{[]string{"||rewrite.example^$dnsrewrite=192.0.2.1,important", "@@||rewrite.example^$dnsrewrite"}, 0, 1},
		{[]string{"||rewrite.example^$dnsrewrite=192.0.2.1", "||rewrite.example^$dnsrewrite=NOERROR"}, 0, 0},
		{[]string{"||rewrite.example^$dnsrewrite=192.0.2.1,important", "||rewrite.example^$dnsrewrite=REFUSED"}, dns.RcodeRefused, 0},
		{[]string{"||rewrite.example^$dnsrewrite=192.0.2.1", "||rewrite.example^$dnsrewrite=target.example"}, 0, 1},
	} {
		d := selectionFor(t, tc.rules...).Evaluate("rewrite.example")
		if d.Action != ActionRewrite || d.Rewrite.Rcode() != tc.code || len(d.Rewrite.Records("rewrite.example.", dns.TypeA, 1, 60)) != tc.records {
			t.Fatalf("rewrite precedence %v: %+v", tc.rules, d)
		}
	}
	sel := selectionFor(t, "||blocked.example^$dnstype=AAAA,important", "@@||blocked.example^", "||rewrite.example^$dnsrewrite=192.0.2.1")
	snap, _ := NewSnapshot(1, sel.block[0])
	ctx := Context{Name: "blocked.example", Type: dns.TypeAAAA}
	if got := snap.ContainsContext(ctx); got["rules"] != "||blocked.example^$dnstype=AAAA,important" {
		t.Fatalf("context diagnostics omit deny: %v", got)
	}
	if got := snap.ContainsAllowContext(ctx); len(got) != 0 {
		t.Fatalf("ordinary exception masked important deny: %v", got)
	}
	ctx.Type = dns.TypeA
	if got := snap.ContainsAllowContext(ctx); got["rules"] != "@@||blocked.example^" {
		t.Fatalf("context diagnostics omit allow: %v", got)
	}
	if len(snap.Contains("rewrite.example")) != 0 {
		t.Fatal("typed rewrite reported as a deny")
	}
}

func TestDenseModifierCandidatesRetainLastImportantRule(t *testing.T) {
	var rules []string
	for i := 0; i < 1500; i++ {
		rules = append(rules, fmt.Sprintf("||dense.example^$client='client%d'", i))
	}
	rules = append(rules, "@@||dense.example^", "||dense.example^$important,ctag=device_pc")
	d := selectionFor(t, rules...).EvaluateContext(Context{Name: "dense.example", Type: dns.TypeA, Tags: []string{"device_pc"}})
	if !d.Blocked() || d.Rule() != rules[len(rules)-1] {
		t.Fatalf("dense index missed final deciding rule: %+v", d)
	}
}

func BenchmarkModifiers(b *testing.B) {
	var rules []string
	for i := 0; i < 10000; i++ {
		rules = append(rules, fmt.Sprintf("||host%d.example^$client=192.0.2.0/24,dnstype=AAAA", i))
	}
	sel := selectionFor(b, rules...)
	for _, name := range []string{"host9999.example", "unrelated.invalid"} {
		b.Run(name, func(b *testing.B) {
			ctx := Context{Name: name, Type: dns.TypeAAAA, ClientIP: netip.MustParseAddr("192.0.2.5")}
			b.ReportAllocs()
			for b.Loop() {
				sinkDecision = sel.EvaluateContext(ctx)
			}
		})
	}
	b.Run("dense-1500", func(b *testing.B) {
		var rules []string
		for i := 0; i < 1500; i++ {
			rules = append(rules, fmt.Sprintf("||dense.example^$client='client%d'", i))
		}
		sel := selectionFor(b, rules...)
		ctx := Context{Name: "dense.example", Type: dns.TypeA, ClientID: "client1499"}
		b.ReportAllocs()
		for b.Loop() {
			sinkDecision = sel.EvaluateContext(ctx)
		}
	})
}
