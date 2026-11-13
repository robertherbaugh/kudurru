package rpz

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// reportLines covers every supported trigger and action plus one owner of
// each ignored kind (D391).
var reportLines = []string{
	"nx.test.policy.test. 300 IN CNAME .",
	"*.wild.test.policy.test. 300 IN CNAME *.",
	"pass.test.policy.test. 300 IN CNAME rpz-passthru.",
	"drop.test.policy.test. 300 IN CNAME rpz-drop.",
	"tcp.test.policy.test. 300 IN CNAME rpz-tcp-only.",
	"local.test.policy.test. 300 IN A 192.0.2.10",
	"32.2.0.0.127.rpz-client-ip.policy.test. 300 IN CNAME .",
	"24.0.2.0.192.rpz-ip.policy.test. 300 IN CNAME *.",
	"ns.example.rpz-nsdname.policy.test. 300 IN CNAME .",
	"32.1.0.0.10.rpz-nsip.policy.test. 300 IN CNAME .",
	"999.rpz-ip.policy.test. 300 IN CNAME .",
	"dname.test.policy.test. 300 IN DNAME elsewhere.test.",
	"odd.test.policy.test. 300 IN CNAME rpz-unknown.",
}

func TestCompileRuleAndIgnoredAccounting(t *testing.T) {
	z := fixtureZone(t, reportLines...)
	want := RuleCounts{QName: 6, ClientIP: 1, ResponseIP: 1, NXDOMAIN: 2, NODATA: 2, Passthru: 1, Drop: 1, TCPOnly: 1, LocalData: 1}
	if got := z.Rules(); got != want {
		t.Fatalf("rule counts = %+v, want %+v", got, want)
	}
	if s := z.Stats(); s.Rules != 8 || s.Unsupported != 4 || s.Invalid != 1 {
		t.Fatalf("stats = %+v", s)
	}
	got := z.Ignored()
	wantIgnored := []IgnoredReason{
		{Reason: "dname-action", Unsupported: true, Count: 1, Examples: []string{"dname.test.policy.test."}},
		{Reason: "invalid-address-trigger", Unsupported: false, Count: 1, Examples: []string{"999.rpz-ip.policy.test."}},
		{Reason: "resolver-ns-provenance", Unsupported: true, Count: 2, Examples: []string{"32.1.0.0.10.rpz-nsip.policy.test.", "ns.example.rpz-nsdname.policy.test."}},
		{Reason: "unknown-reserved-action", Unsupported: true, Count: 1, Examples: []string{"odd.test.policy.test."}},
	}
	if !reflect.DeepEqual(got, wantIgnored) {
		t.Fatalf("ignored = %+v", got)
	}
	got[0].Examples[0] = "mutated."
	if z.Ignored()[0].Examples[0] != "dname.test.policy.test." {
		t.Fatal("Ignored returned shared examples")
	}
}

// Counts stay complete beyond the sample cap; examples stay bounded.
func TestIgnoredAccountingBeyondDiagnosticCap(t *testing.T) {
	lines := make([]string, 0, MaxDiagnostics+20)
	for i := range MaxDiagnostics + 20 {
		lines = append(lines, "n"+strings.Repeat("x", i%7)+"."+itoaTest(i)+".rpz-nsdname.policy.test. 300 IN CNAME .")
	}
	lines = append(lines, "zzz.rpz-ip.policy.test. 300 IN CNAME .")
	z := fixtureZone(t, lines...)
	ignored := z.Ignored()
	if len(ignored) != 2 || ignored[1].Reason != "resolver-ns-provenance" || ignored[1].Count != MaxDiagnostics+20 || len(ignored[1].Examples) != MaxIgnoredExamples {
		t.Fatalf("capped accounting = %+v", ignored)
	}
	// The single invalid owner sorts after every nsdname sample, so the cap
	// dropped its sample; its classification must still be malformed.
	if ignored[0].Reason != "invalid-address-trigger" || ignored[0].Unsupported || ignored[0].Count != 1 {
		t.Fatalf("classification without a sample = %+v", ignored[0])
	}
}

func itoaTest(i int) string {
	const digits = "abcdefghij"
	if i == 0 {
		return "a"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}

type reportReceiver struct{ lines []string }

func (r reportReceiver) records() []dns.RR {
	var out []dns.RR
	body := append([]string{"policy.test. 600 IN SOA ns.policy.test. hostmaster.policy.test. 77 10 3 30 5", "policy.test. 600 IN NS ns.policy.test."}, r.lines...)
	for _, line := range body {
		rr, err := dns.NewRR(line)
		if err != nil {
			panic(err)
		}
		out = append(out, rr)
	}
	return out
}
func (r reportReceiver) SOA(context.Context, *v1alpha1.DNSZone, string) (*dns.SOA, error) {
	return r.records()[0].(*dns.SOA), nil
}
func (r reportReceiver) AXFR(context.Context, *v1alpha1.DNSZone, string) ([]dns.RR, error) {
	return r.records(), nil
}

func TestStatusesReportAcceptedCopyAndListLookup(t *testing.T) {
	fx := newRuntime(t, reportReceiver{lines: reportLines}, runtimeDB(t), runtimeList())
	if _, err := fx.m.ListStatuses("default", "absent"); !errors.Is(err, ErrListNotConfigured) {
		t.Fatalf("absent list error = %v", err)
	}
	pending, err := fx.m.ListStatuses("default", "ads")
	if err != nil || len(pending) != 1 || pending[0].Available || pending[0].IgnoredReasons() != nil || pending[0].Source != "rpz-0" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	stop := startRuntime(t, fx.m)
	st := awaitStatus(t, fx, func(s Status) bool { return s.Available })
	if st.Serial != 77 || st.Rules.ResponseIP != 1 || st.Stats.Unsupported != 4 || len(st.IgnoredReasons()) != 4 ||
		!st.ExpiresAt.Equal(st.ConfirmedAt.Add(30*time.Second)) || st.NextRefresh.IsZero() {
		t.Fatalf("serving status = %+v", st)
	}
	listed, err := fx.m.ListStatuses("default", "ads")
	if err != nil || len(listed) != 1 || listed[0].Serial != 77 || !reflect.DeepEqual(listed[0].IgnoredReasons(), st.IgnoredReasons()) {
		t.Fatalf("list statuses = %+v, %v", listed, err)
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(NewCollector(fx.m))
	value := func(name, reason string) (float64, bool) {
		t.Helper()
		families, err := reg.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() != name {
				continue
			}
			for _, m := range f.Metric {
				labels := map[string]string{}
				for _, l := range m.Label {
					labels[l.GetName()] = l.GetValue()
				}
				if labels["list"] == "default/ads" && labels["source"] == "rpz-0" && labels["reason"] == reason {
					return m.GetGauge().GetValue(), true
				}
			}
		}
		return 0, false
	}
	for _, c := range []struct {
		name, reason string
		want         float64
	}{
		{"kudurru_rpz_feed_available", "", 1}, {"kudurru_rpz_feed_serial", "", 77}, {"kudurru_rpz_feed_rules", "", 8},
		{"kudurru_rpz_feed_ignored_rules", "resolver-ns-provenance", 2}, {"kudurru_rpz_feed_expires_timestamp_seconds", "", float64(st.ExpiresAt.Unix())},
	} {
		if got, ok := value(c.name, c.reason); !ok || got != c.want {
			t.Fatalf("%s{%s} = %v (%v), want %v", c.name, c.reason, got, ok, c.want)
		}
	}
	// Expiry is visible at scrape time, between scheduler ticks.
	stop()
	fx.clock.add(31 * time.Second)
	if got, _ := value("kudurru_rpz_feed_available", ""); got != 0 {
		t.Fatalf("expired availability = %v", got)
	}
	if expired, _ := fx.m.ListStatuses("default", "ads"); expired[0].Available || expired[0].ConfirmedAt.IsZero() {
		t.Fatalf("expired status = %+v", expired[0])
	}
}
