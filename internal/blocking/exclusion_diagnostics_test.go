package blocking

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
)

func TestExcludedQuestionRulesOriginalSourceAndContext(t *testing.T) {
	rules := []string{"exact.example", "||suffix.example^", `/^r[0-9]+\.example$/`, "@@||except.example^", "||typed.example^$dnstype=AAAA,client=192.0.2.7", "||rewrite.example^$dnsrewrite=192.0.2.8"}
	src := LinesSource("https://source.invalid", parser.FormatAdblock, rules)
	src.Exclude = append(slices.Clone(rules), "missing.example")
	list := mustCompile(t, Spec{Name: "default/blocked", Mode: ModeBlock, Sources: []Source{src}})
	allow := mustCompile(t, Spec{Name: "default/allow", Mode: ModeAllow, Sources: []Source{src}})
	sel := &Selection{block: []*List{list}}
	for _, test := range []struct {
		name, effect string
		typ          uint16
		client       string
		want         bool
	}{
		{"EXACT.example.", "block", dns.TypeA, "192.0.2.7", true},
		{"child.exact.example", "", dns.TypeA, "192.0.2.7", false},
		{"child.suffix.example", "block", dns.TypeA, "192.0.2.7", true},
		{"r99.example", "block", dns.TypeA, "192.0.2.7", true},
		{"except.example", "exception", dns.TypeA, "192.0.2.7", true},
		{"typed.example", "block", dns.TypeAAAA, "192.0.2.7", true},
		{"typed.example", "", dns.TypeA, "192.0.2.7", false},
		{"typed.example", "", dns.TypeAAAA, "192.0.2.9", false},
		{"rewrite.example", "rewrite", dns.TypeA, "192.0.2.7", true},
		{"missing.example", "", dns.TypeA, "192.0.2.7", false},
	} {
		ctx := Context{Name: test.name, Type: test.typ, Class: dns.ClassINET, ClientIP: netip.MustParseAddr(test.client)}
		matches, partial := sel.ExcludedQuestionRules(t.Context(), ctx)
		if partial || (len(matches) == 1) != test.want || len(matches) > 1 {
			t.Fatalf("%s %d %s: matches=%v partial=%v", test.name, test.typ, test.client, matches, partial)
		}
		if test.want && (matches[0].Effect != test.effect || matches[0].Source != 0 || matches[0].List != list.Name()) {
			t.Fatal("wrong original rule attribution", matches)
		}
		if got := sel.EvaluateContext(ctx); got.Action != ActionPass {
			t.Fatal("diagnostic exclusion affected filtering", got)
		}
	}
	matches, partial := (&Selection{allow: []*List{allow}}).ExcludedQuestionRules(t.Context(), Context{Name: "except.example", Type: dns.TypeA, Class: dns.ClassINET})
	if partial || len(matches) != 1 || matches[0].Effect != "allow" {
		t.Fatal("allow-list exclusion misrepresented", matches, partial)
	}
	// Changing exclusions produces a new immutable List. The held old List
	// still reports its original source evidence, even after reuse of buffers.
	src.Exclude = nil
	newList, err := Compile(Spec{Name: list.Name(), Mode: ModeBlock, Sources: []Source{src}}, list, CompileOptions{})
	if err != nil || newList == list {
		t.Fatal("changed excludes reused original list", err)
	}
	for _, test := range []struct {
		l     *List
		count int
	}{{list, 1}, {newList, 0}} {
		out, _ := (&Selection{block: []*List{test.l}}).ExcludedQuestionRules(t.Context(), Context{Name: "exact.example", Type: 1, Class: 1})
		if len(out) != test.count {
			t.Fatal("exclusion evidence crossed immutable lists")
		}
	}
}

func TestExcludedQuestionRulesBudgetsAndCancellation(t *testing.T) {
	var lines []string
	for i := range maxDiagnosticExclusions + 1 {
		lines = append(lines, fmt.Sprintf("host%d.example", i))
	}
	src := LinesSource("source", parser.FormatDomains, lines)
	src.Exclude = lines
	list := mustCompile(t, Spec{Name: "bounded", Mode: ModeBlock, Sources: []Source{src}})
	if len(list.exclusions.rules) != maxDiagnosticExclusions || !list.exclusions.truncated {
		t.Fatal("compile witness count was unbounded")
	}
	sel := &Selection{block: []*List{list}}
	if _, partial := sel.ExcludedQuestionRules(t.Context(), Context{Name: "host0.example"}); !partial {
		t.Fatal("compile truncation hidden")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if out, partial := sel.ExcludedQuestionRules(canceled, Context{Name: "host0.example"}); len(out) != 0 || !partial {
		t.Fatal("canceled diagnostic scanned rules")
	}
	// Many selected lists have the same matching excluded rule: output is bounded.
	sel.block = slices.Repeat([]*List{list}, MaxExclusionMatches+1)
	if out, partial := sel.ExcludedQuestionRules(t.Context(), Context{Name: "host0.example"}); len(out) > MaxExclusionMatches || !partial {
		t.Fatal("query evidence budget missing")
	}
	var set exclusionSet
	set.addBasic(0, parser.Rule{Kind: parser.KindRegex, Syntax: parser.SyntaxRegex, Pattern: strings.Repeat("x", maxDiagnosticRule)})
	if len(set.rules) != 0 || !set.truncated {
		t.Fatal("oversized witness retained")
	}
}
