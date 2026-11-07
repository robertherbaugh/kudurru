package blocking

import (
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestSelectionBadfilterCannotEditExplicitAllowEngine(t *testing.T) {
	for _, rule := range []string{"||permit.example^", "||permit.example^$dnstype=A,client=192.0.2.5"} {
		allow, err := Compile(Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{rule})}}, nil, CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cancel := rule + "$badfilter"
		if strings.Contains(rule, "$") {
			cancel = rule + ",badfilter"
		}
		block, err := Compile(Spec{Name: "block", Mode: ModeBlock, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"||permit.example^$important", cancel})}}, nil, CompileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		snap, _ := NewSnapshot(1, allow, block)
		sel, _ := snap.Select([]string{"block", "allow"}, SelectOptions{ExceptionScope: ScopeSelection})
		ctx := Context{Name: "permit.example", Type: dns.TypeA, ClientIP: netip.MustParseAddr("192.0.2.5")}
		if d := sel.EvaluateContext(ctx); d.Reason != ReasonAllowList || d.List != "allow" {
			t.Fatalf("explicit allow engine canceled by block list: %+v", d)
		}
	}
	allow, _ := Compile(Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"192.0.2.66"})}}, nil, CompileOptions{})
	block, _ := Compile(Spec{Name: "block", Mode: ModeBlock, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"192.0.2.66$badfilter", "||192.0.2.66^$important"})}}, nil, CompileOptions{})
	snap, _ := NewSnapshot(1, allow, block)
	sel, _ := snap.Select([]string{"block", "allow"}, SelectOptions{ExceptionScope: ScopeSelection})
	if sel.EvaluateResponse(nil, []netip.Addr{netip.MustParseAddr("192.0.2.66")}).Blocked() {
		t.Fatal("IP authorization canceled by block list")
	}
}

func TestAddressBadfilterAndImmutableDiagnostics(t *testing.T) {
	for _, rule := range []string{"192.0.2.66", "||192.0.2.66^", "|192.0.2.66|"} {
		sel := selectionFor(t, rule, rule+"$badfilter")
		if sel.EvaluateResponse(nil, []netip.Addr{netip.MustParseAddr("192.0.2.66")}).Blocked() {
			t.Fatalf("IP badfilter failed for %s", rule)
		}
	}
	list := selectionFor(t, "||geo.example^$respgeo=US", "||bad.example^$unknown", "/.{1,4}/$dnstype=A").block[0]
	if list.Stats().RegexUnindexed != 1 {
		t.Fatal("modifier regex index accounting omitted")
	}
	sources := list.Sources()
	sources[0].InvalidReasons[parser.ReasonUnknownModifier] = 999
	sources[0].UnsupportedReasons[parser.ReasonModifiers] = 999
	sources[0].InvalidSamples[0].Text = "changed"
	unsupported := list.Unsupported()
	unsupported[0].Modifiers[0].Value = "changed"
	if list.Sources()[0].InvalidReasons[parser.ReasonUnknownModifier] != 1 || list.Sources()[0].UnsupportedReasons[parser.ReasonModifiers] != 1 || list.Sources()[0].InvalidSamples[0].Text == "changed" || list.Unsupported()[0].Modifiers[0].Value != "US" {
		t.Fatal("diagnostic caller mutated immutable list")
	}
	allow, err := Compile(Spec{Name: "allow", Mode: ModeAllow, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"@@||allow.example^$important"})}}, nil, CompileOptions{})
	if err != nil || allow.Stats().Exceptions != 0 {
		t.Fatal("allow-mode exceptions were not folded into primary entries")
	}
}

func TestTypedRewriteInvalidRDATAAndQuotedText(t *testing.T) {
	for _, value := range []string{
		"NOTIMP", "NOERROR;OPT;1232", "NOERROR;DNSKEY;256 3 15 AAAA", "NXDOMAIN;A;192.0.2.1", "NOERROR;A;not-an-ip", "NOERROR;CNAME;.", "NOERROR;CNAME;bad..example", "NOERROR;MX;10 bad..example", "NOERROR;SRV;1 2 3 bad..example", "NOERROR;HTTPS;1 bad..example", "NOERROR;CNAME;" + strings.Repeat("x", 64) + ".example", "NOERROR;TXT;",
	} {
		sel := selectionFor(t, "||invalid.example^$dnsrewrite="+value)
		if list := sel.block[0]; list.Stats().Entries != 0 || list.Stats().Invalid != 1 || sel.Evaluate("invalid.example").Action != ActionPass {
			t.Errorf("invalid rewrite broadened: %s %+v", value, list.Stats())
		}
	}
	for _, value := range []string{`NOERROR;TXT;hello\,world`, `NOERROR;TXT;"hello,world"`} {
		d := selectionFor(t, "||text.example^$dnsrewrite="+value).EvaluateContext(Context{Name: "text.example", Type: dns.TypeTXT})
		if d.Action != ActionRewrite {
			t.Fatalf("TXT rejected: %s %+v", value, d)
		}
		rrs := d.Rewrite.Records("text.example.", dns.TypeTXT, dns.ClassINET, 60)
		if len(rrs) != 1 || len(rrs[0].(*dns.TXT).Txt) != 1 || rrs[0].(*dns.TXT).Txt[0] != "hello,world" {
			t.Fatalf("TXT altered: %v", rrs)
		}
	}
}

func TestRewriteOrderIndependentOfShortcutTraversal(t *testing.T) {
	first := "alpha*bravo$dnsrewrite=NXDOMAIN"
	sel := selectionFor(t, first, "bravo*alpha$dnsrewrite=REFUSED")
	if d := sel.Evaluate("bravo.alpha.bravo"); d.Rule() != first || d.Rewrite.Rcode() != dns.RcodeNameError {
		t.Fatalf("rewrite tie changed source order: %+v", d)
	}
	sel = selectionFor(t, "alpha*bravo$dnsrewrite=192.0.2.1", "bravo*alpha$dnsrewrite=192.0.2.2")
	d := sel.Evaluate("bravo.alpha.bravo")
	rrs := d.Rewrite.Records("bravo.alpha.bravo.", dns.TypeA, 1, 60)
	if len(rrs) != 2 || rrs[0].(*dns.A).A.String() != "192.0.2.1" {
		t.Fatal("accumulation order followed query grams")
	}
}

func TestModifierSnapshotConcurrentReloadAndPrivateMaterialization(t *testing.T) {
	old := selectionFor(t, "||rewrite.example^$ctag=device_pc,dnsrewrite=192.0.2.1")
	snapshot, _ := NewSnapshot(1, old.block[0])
	var current atomic.Pointer[Snapshot]
	current.Store(snapshot)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 150 {
				sel, err := current.Load().Select([]string{"rules"}, SelectOptions{})
				if err != nil {
					t.Error(err)
					return
				}
				d := sel.EvaluateContext(Context{Name: "rewrite.example", Type: dns.TypeA, Tags: []string{"device_pc"}})
				if d.Action != ActionRewrite {
					t.Error("snapshot missed rewrite")
					return
				}
				rrs := d.Rewrite.Records("rewrite.example.", 1, 1, 60)
				rrs[0].(*dns.A).A.To4()[3] = 99
				if got := d.Rewrite.Records("rewrite.example.", 1, 1, 60)[0].(*dns.A).A.String(); got != "192.0.2.1" && got != "192.0.2.2" {
					t.Error("materialized RR changed shared plan")
					return
				}
			}
		})
	}
	for i := range 12 {
		address := "192.0.2.1"
		if i%2 == 0 {
			address = "192.0.2.2"
		}
		spec := Spec{Name: "rules", Mode: ModeBlock, Sources: []Source{LinesSource("inline", parser.FormatAdblock, []string{"||rewrite.example^$ctag=device_pc,dnsrewrite=" + address})}}
		next, reports := Rebuild(current.Load(), []Spec{spec}, CompileOptions{})
		if reports[0].Err != nil {
			t.Fatal(reports[0].Err)
		}
		current.Store(next)
	}
	wg.Wait()
	if got := old.EvaluateContext(Context{Name: "rewrite.example", Type: dns.TypeA, Tags: []string{"device_pc"}}).Rewrite.Records("rewrite.example.", 1, 1, 60)[0].(*dns.A).A.String(); got != "192.0.2.1" {
		t.Fatal("reload changed an in-flight selection")
	}
}
