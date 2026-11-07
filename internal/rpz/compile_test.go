package rpz

import (
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

var testNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

func fixtureRecords(t testing.TB, origin string, lines ...string) []dns.RR {
	t.Helper()
	body := []string{origin + " 600 IN SOA ns." + origin + " hostmaster." + origin + " 4294967294 30 10 60 5", origin + " 600 IN NS ns." + origin}
	body = append(body, lines...)
	var records []dns.RR
	for _, line := range body {
		rr, err := dns.NewRR(line)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rr)
	}
	return records
}

func fixtureZone(t testing.TB, lines ...string) *Zone {
	t.Helper()
	z, err := Compile(Spec{List: "default/ads", Source: "feed", Origin: "policy.test.", Identity: "identity-a", ExpiresAt: testNow.Add(time.Minute)}, fixtureRecords(t, "policy.test.", lines...))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestActionsAndPrivateRawData(t *testing.T) {
	for _, tc := range []struct {
		target string
		action Action
	}{
		{".", ActionNXDOMAIN}, {"*.", ActionNODATA}, {"rpz-passthru.", ActionPassthru},
		{"rpz-drop.", ActionDrop}, {"rpz-tcp-only.", ActionTCPOnly}, {"garden.test.", ActionLocalData},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			input := fixtureRecords(t, "policy.test.", "bad.test.policy.test. 300 IN CNAME "+tc.target)
			z, err := Compile(Spec{List: "ads", Source: "feed", Origin: "POLICY.TEST", Identity: "a", ExpiresAt: testNow.Add(time.Minute)}, input)
			if err != nil {
				t.Fatal(err)
			}
			input[0].(*dns.SOA).Serial = 1
			input[2].(*dns.CNAME).Target = "changed.test."
			decision := NewSelection(z).Evaluate(Context{Name: "BAD.TEST", Type: dns.TypeA}, testNow)
			if decision.Action != tc.action || decision.Owner != "bad.test.policy.test." || decision.Trigger != TriggerQName {
				t.Fatalf("decision = %+v", decision)
			}
			if z.SOA().Serial != 4294967294 || z.Records()[0].Header().Ttl != 600 {
				t.Fatal("external serial/TTL changed")
			}
			raw, soa := z.Records(), z.SOA()
			raw[2].(*dns.CNAME).Target = "changed-again.test."
			soa.Serial = 2
			if z.SOA().Serial != 4294967294 || z.Records()[2].(*dns.CNAME).Target != tc.target {
				t.Fatal("returned raw data mutates the compiled zone")
			}
		})
	}
	// SOA need not be first in input; the IXFR accessor places it first.
	raw := fixtureRecords(t, "policy.test.")
	raw[0], raw[1] = raw[1], raw[0]
	z, err := Compile(Spec{List: "ads", Source: "feed", Origin: "policy.test.", Identity: "a", ExpiresAt: testNow.Add(time.Minute)}, raw)
	if err != nil || z.Records()[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("SOA placement: %v", err)
	}
}

func TestLocalDataAndLifetime(t *testing.T) {
	z := fixtureZone(t,
		"data.test.policy.test. 300 IN A 192.0.2.1",
		"data.test.policy.test. 300 IN A 192.0.2.2",
		"data.test.policy.test. 400 IN TXT \"first-party\"",
		"wild.test.policy.test. 300 IN CNAME *.garden.test.",
		"unknown.test.policy.test. 300 IN TYPE65400 \\# 3 000102",
	)
	d := NewSelection(z).Evaluate(Context{Name: "data.test.", Type: dns.TypeA}, testNow.Add(30*time.Second))
	records, err := d.Records("data.test", dns.TypeA, 20)
	if err != nil || len(records) != 2 || records[0].Header().Ttl != 20 {
		t.Fatalf("materialized records=%v err=%v", records, err)
	}
	records[0].(*dns.A).A[0] = 1
	again, err := d.Records("data.test.", dns.TypeA, 0)
	if err != nil || again[0].(*dns.A).A.String() != "192.0.2.1" || again[0].Header().Ttl != 30 {
		t.Fatal("RR materialization is mutable or exceeds absolute expiry")
	}
	if other, err := d.Records("data.test.", dns.TypeAAAA, 0); err != nil || len(other) != 0 {
		t.Fatalf("absent family should be NODATA: %v %v", other, err)
	}
	wild := NewSelection(z).Evaluate(Context{Name: "wild.test."}, testNow)
	answer, err := wild.Records("wild.test.", dns.TypeAAAA, 0)
	if err != nil || answer[0].(*dns.CNAME).Target != "wild.test.garden.test." {
		t.Fatalf("wildcard CNAME = %v %v", answer, err)
	}
	long := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 50) + "."
	if answer, err := wild.Records(long, dns.TypeA, 0); err == nil || answer != nil {
		t.Fatal("oversized wildcard target accepted")
	}
	unknown := NewSelection(z).Evaluate(Context{Name: "unknown.test."}, testNow)
	answer, err = unknown.Records("unknown.test.", 65400, 0)
	if err != nil || len(answer) != 1 || answer[0].(*dns.RFC3597).Rdata != "000102" {
		t.Fatalf("opaque local data lost: %v %v", answer, err)
	}
}

func TestTriggerPriorityAndAddressEncoding(t *testing.T) {
	z := fixtureZone(t,
		"bad.test.policy.test. 60 IN CNAME .",
		"24.0.2.0.192.rpz-client-ip.policy.test. 60 IN CNAME .",
		"32.5.2.0.192.rpz-client-ip.policy.test. 60 IN CNAME rpz-passthru.",
		"32.7.2.0.192.rpz-ip.policy.test. 60 IN CNAME *.",
		"32.6.2.0.192.rpz-ip.policy.test. 60 IN CNAME rpz-drop.",
		"24.0.2.0.192.rpz-ip.policy.test. 60 IN CNAME .",
		"64.zz.db8.2001.rpz-client-ip.policy.test. 60 IN CNAME rpz-tcp-only.",
		"128.9.zz.db8.2001.rpz-ip.policy.test. 60 IN CNAME .",
	)
	for _, tc := range []struct {
		client, name string
		answers      []string
		action       Action
		trigger      Trigger
		matched      string
	}{
		{"192.0.2.5", "bad.test.", nil, ActionPassthru, TriggerClientIP, "192.0.2.5"},
		{"192.0.2.8", "bad.test.", nil, ActionNXDOMAIN, TriggerClientIP, "192.0.2.8"},
		{"198.51.100.1", "bad.test.", []string{"192.0.2.7"}, ActionNXDOMAIN, TriggerQName, "bad.test."},
		{"198.51.100.1", "good.test.", []string{"192.0.2.7", "192.0.2.6"}, ActionDrop, TriggerResponseIP, "192.0.2.6"},
		{"2001:db8::1", "good.test.", nil, ActionTCPOnly, TriggerClientIP, "2001:db8::1"},
		{"2001:db9::1", "good.test.", []string{"2001:db8::9"}, ActionNXDOMAIN, TriggerResponseIP, "2001:db8::9"},
	} {
		ctx := Context{Name: tc.name, ClientIP: netip.MustParseAddr(tc.client)}
		for _, a := range tc.answers {
			ctx.Addresses = append(ctx.Addresses, netip.MustParseAddr(a))
		}
		d := NewSelection(z).Evaluate(ctx, testNow)
		if d.Action != tc.action || d.Trigger != tc.trigger || d.Matched != tc.matched {
			t.Errorf("%+v: %+v", tc, d)
		}
	}
}

func TestQNameClosestEncloserAndAlias(t *testing.T) {
	z := fixtureZone(t,
		"*.example.policy.test. 60 IN CNAME .",
		"allowed.example.policy.test. 60 IN CNAME rpz-passthru.",
		"leaf.empty.example.policy.test. 60 IN CNAME *.",
	)
	for name, action := range map[string]Action{
		"example.": ActionNone, "child.example.": ActionNXDOMAIN,
		"deep.child.example.": ActionNXDOMAIN, "allowed.example.": ActionPassthru,
		"child.allowed.example.": ActionNone, "other.empty.example.": ActionNone,
	} {
		if got := NewSelection(z).Evaluate(Context{Name: name}, testNow); got.Action != action {
			t.Errorf("%s = %s, want %s", name, got.Action, action)
		}
	}
	if d := NewSelection(z).Evaluate(Context{Name: "original.test.", Names: []string{"CHILD.EXAMPLE."}}, testNow); d.Action != ActionNXDOMAIN || d.Matched != "child.example." {
		t.Fatalf("CNAME target not evaluated: %+v", d)
	}
}

func TestOrderedFeedsPlaceholdersAndExpiry(t *testing.T) {
	first := fixtureZone(t, "32.7.2.0.192.rpz-ip.policy.test. 60 IN CNAME rpz-passthru.")
	second := fixtureZone(t, "bad.test.policy.test. 60 IN CNAME .")
	ctx := Context{Name: "bad.test.", Addresses: []netip.Addr{netip.MustParseAddr("192.0.2.7")}}
	if d := NewSelection(first, second).Evaluate(ctx, testNow); d.Action != ActionPassthru {
		t.Fatal("earlier response-IP feed lost to later QNAME")
	}
	if d := NewSelection(second, first).Evaluate(ctx, testNow); d.Action != ActionNXDOMAIN {
		t.Fatal("declared feed order ignored")
	}
	placeholder, err := Compile(Spec{List: "required", Source: "feed", Origin: "policy.test.", Identity: "new-identity"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if placeholder.AvailableAt(testNow) || !first.AvailableAt(testNow.Add(time.Minute-time.Nanosecond)) || first.AvailableAt(testNow.Add(time.Minute)) {
		t.Fatal("placeholder or exact expiry boundary incorrect")
	}
	if d := NewSelection(first, placeholder).Evaluate(ctx, testNow); !d.Unavailable || d.Identity != "new-identity" || d.Rcode() != dns.RcodeServerFailure {
		t.Fatalf("missing required later feed failed open: %+v", d)
	}
	if d := NewSelection(first).Evaluate(ctx, testNow.Add(time.Minute)); !d.Unavailable {
		t.Fatal("expired feed applied stale policy")
	}
	if d := NewSelection(first).Evaluate(Context{Name: "bad.test.", Class: dns.ClassCHAOS}, testNow.Add(time.Minute)); d.Unavailable || d.Action != ActionNone {
		t.Fatal("RPZ modified non-IN operation")
	}
}

func TestInvalidStructureAndCompleteDiagnostics(t *testing.T) {
	base := fixtureRecords(t, "policy.test.")
	for _, tc := range []struct {
		name string
		rrs  []dns.RR
	}{
		{"empty", []dns.RR{}}, {"missing SOA", base[1:]}, {"closing SOA", append(slicesRR(base), base[0])},
		{"no NS", base[:1]}, {"nil RR", append(slicesRR(base), nil)},
		{"CNAME coexistence", fixtureRecords(t, "policy.test.", "bad.policy.test. IN CNAME .", "bad.policy.test. IN A 192.0.2.1")},
		{"multiple CNAME", fixtureRecords(t, "policy.test.", "bad.policy.test. IN CNAME .", "bad.policy.test. IN CNAME *.")},
		{"out of zone", fixtureRecords(t, "policy.test.", "other.test. IN CNAME .")},
		{"wrong class", fixtureRecords(t, "policy.test.", "bad.policy.test. CH TXT \"x\"")},
		{"empty A", append(slicesRR(base), &dns.A{Hdr: dns.RR_Header{Name: "bad.policy.test.", Rrtype: dns.TypeA, Class: dns.ClassINET}})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if z, err := Compile(Spec{List: "ads", Source: "feed", Origin: "policy.test.", Identity: "a", ExpiresAt: testNow.Add(time.Minute)}, tc.rrs); err == nil || z != nil {
				t.Fatal("accepted structurally invalid policy zone")
			}
		})
	}
	var lines []string
	for i := 0; i < MaxDiagnostics+100; i++ {
		lines = append(lines, fmt.Sprintf("ns%d.rpz-nsdname.policy.test. IN CNAME .", i))
	}
	lines = append(lines, "last.test.policy.test. IN CNAME .",
		"32.0.2.192.rpz-ip.policy.test. IN CNAME .",   // Incomplete reversed IPv4.
		"24.7.2.0.192.rpz-ip.policy.test. IN CNAME .", // Unmasked host bits.
		"bad.rpz-unknown.policy.test. IN CNAME .",
		"unknown.test.policy.test. IN CNAME rpz-future.",
		"middle.*.test.policy.test. IN CNAME .",
		"dname.test.policy.test. IN DNAME target.test.",
	)
	z := fixtureZone(t, lines...)
	if got := z.Stats(); got.Rules != 1 || got.Unsupported != 1104 || got.Invalid != 2 || len(z.Diagnostics()) != MaxDiagnostics {
		t.Fatalf("counts = %+v diagnostics=%d", got, len(z.Diagnostics()))
	}
	if d := NewSelection(z).Evaluate(Context{Name: "last.test."}, testNow); d.Action != ActionNXDOMAIN {
		t.Fatal("diagnostic sample cap truncated execution")
	}
	items := z.Diagnostics()
	items[0].Reason = "changed"
	entries := z.Entries()
	entries[0].Action = ActionDrop
	if z.Diagnostics()[0].Reason == "changed" || z.Entries()[0].Action == ActionDrop {
		t.Fatal("diagnostic values are mutable")
	}
}

func slicesRR(records []dns.RR) []dns.RR { return append([]dns.RR(nil), records...) }

func TestExclusionsAndConcurrentMaterialization(t *testing.T) {
	spec := Spec{List: "ads", Source: "feed", Origin: "policy.test.", Identity: "a", ExpiresAt: testNow.Add(time.Minute), Exclude: []string{"OFF.TEST.POLICY.TEST", "missing.policy.test."}}
	z, err := Compile(spec, fixtureRecords(t, "policy.test.", "off.test.policy.test. IN CNAME .", "on.test.policy.test. IN A 192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	spec.Exclude[0] = "on.test.policy.test."
	if got := z.Stats(); got.Excluded != 1 || got.StaleExcludes != 1 {
		t.Fatalf("exclusion counts = %+v", got)
	}
	selection := NewSelection(z)
	if selection.Evaluate(Context{Name: "off.test."}, testNow).Action != ActionNone {
		t.Fatal("excluded rule still applied")
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 100 {
				d := selection.Evaluate(Context{Name: "on.test.", Type: dns.TypeA}, testNow)
				answer, err := d.Records("on.test.", dns.TypeA, 1)
				if err != nil || len(answer) != 1 || answer[0].(*dns.A).A.String() != "192.0.2.1" {
					t.Error("concurrent materialization changed")
					return
				}
				answer[0].(*dns.A).A[0] = 1
			}
		})
	}
	workers.Wait()
}

func BenchmarkRPZEvaluate(b *testing.B) {
	var lines []string
	for i := range 10000 {
		lines = append(lines, fmt.Sprintf("host%d.test.policy.test. IN CNAME .", i))
	}
	selection := NewSelection(fixtureZone(b, lines...))
	for _, name := range []string{"host9999.test.", "unrelated.test."} {
		b.Run(name, func(b *testing.B) {
			ctx := Context{Name: name, Type: dns.TypeA}
			b.ReportAllocs()
			for b.Loop() {
				_ = selection.Evaluate(ctx, testNow)
			}
		})
	}
}

func BenchmarkRPZCompile(b *testing.B) {
	lines := make([]string, 20000)
	for i := range lines {
		lines[i] = fmt.Sprintf("host%d.test.policy.test. 300 IN CNAME .", i)
	}
	records := fixtureRecords(b, "policy.test.", lines...)
	spec := Spec{List: "default/ads", Source: "feed", Origin: "policy.test.", Identity: "benchmark", ExpiresAt: testNow.Add(time.Minute)}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		z, err := Compile(spec, records)
		if err != nil || z.Stats().Rules != len(lines) {
			b.Fatalf("complete compilation: %v", err)
		}
	}
}

func BenchmarkRPZAddressEvaluate(b *testing.B) {
	selection := NewSelection(fixtureZone(b,
		"24.0.2.0.192.rpz-client-ip.policy.test. 300 IN CNAME .",
		"64.zz.db8.2001.rpz-ip.policy.test. 300 IN CNAME ."))
	for _, tc := range []struct {
		name string
		ctx  Context
	}{
		{"client IPv4", Context{Name: "unrelated.test.", ClientIP: netip.MustParseAddr("192.0.2.17")}},
		{"answer IPv6", Context{Name: "unrelated.test.", Addresses: []netip.Addr{netip.MustParseAddr("2001:db8::17")}}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = selection.Evaluate(tc.ctx, testNow)
			}
		})
	}
}
