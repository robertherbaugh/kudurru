package record

import (
	"encoding/json"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

func fullEvent() *querylog.Event {
	return &querylog.Event{
		ID: 42, Time: time.Date(2026, 11, 10, 12, 34, 56, 789012345, time.FixedZone("CET", 3600)), Latency: 1234567 * time.Nanosecond,
		Node: "node-a", Proto: pipeline.ProtoDoH3, Client: netip.MustParseAddr("192.0.2.7"), Name: "Ads.Example.COM.", QType: 65, QClass: 1,
		Status: pipeline.StatusBlockedCNAME, Rcode: 3, Upstream: "tls://dns.example", List: "default/ads", Rule: "||ads.example.com^", Answers: 2,
		Flags:      querylog.Flags{RD: true, DO: true, Cached: true, ECS: true, Traceparent: true},
		ClientName: "laptop", ClientGroup: "kids", Policy: "default/strict", Zone: "Example.COM.", CNAME: "Tracker.Example.NET.",
		ReplyType: querylog.ReplyNXDomain, DNSSEC: querylog.DNSSECSecure, EDE: 15, HasEDE: true, ECS: netip.MustParsePrefix("198.51.100.0/24"),
		TraceID: [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		Answer:  "tracker.example.net.",
	}
}

// TestRecordIsTheDocumentedRow pins the exported form: the names, the JSON
// types and the encodings are a contract with tables and queries outside
// this repository.
func TestRecordIsTheDocumentedRow(t *testing.T) {
	raw, err := json.Marshal(From(fullEvent()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ts": "2026-11-10T11:34:56.789012Z", "id": float64(42), "node": "node-a", "proto": "doh3",
		"client": "192.0.2.7", "client_name": "laptop", "client_group": "kids",
		"domain": "ads.example.com", "qtype": "HTTPS", "qclass": float64(1),
		"status": "blocked_cname", "status_class": "blocked", "rcode": "NXDOMAIN", "reply_type": "nxdomain",
		"answers": float64(2), "answer": "tracker.example.net.", "latency_us": float64(1234),
		"upstream": "tls://dns.example", "zone": "example.com", "list": "default/ads", "rule": "||ads.example.com^",
		"policy": "default/strict", "cname": "tracker.example.net", "dnssec": "secure", "ede": float64(15),
		"ecs": "198.51.100.0/24", "trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
		"flags": []any{"rd", "do", "cached", "ecs", "traceparent"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record:\n got %v\nwant %v", got, want)
	}
	if at, err := Time(From(fullEvent()).TS); err != nil || !at.Equal(fullEvent().Time.Truncate(time.Microsecond)) {
		t.Fatalf("ts does not round-trip: %v %v", at, err)
	}
}

// TestRecordLeavesOutWhatTheEventDoesNotHave: optional fields are absent,
// never empty strings, zero addresses or a zero trace ID.
func TestRecordLeavesOutWhatTheEventDoesNotHave(t *testing.T) {
	raw, err := json.Marshal(From(&querylog.Event{Time: time.Unix(1, 0), Name: ".", QType: 65280, Rcode: 99, Status: pipeline.StatusForwarded}))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"ts":"1970-01-01T00:00:01.000000Z","id":0,"node":"","proto":"udp","domain":".","qtype":"TYPE65280","qclass":1,` +
		`"status":"forwarded","status_class":"permitted","rcode":"RCODE99","reply_type":"unknown","answers":0,"latency_us":0,"dnssec":"unknown"}`
	if string(raw) != want {
		t.Fatalf("record:\n got %s\nwant %s", raw, want)
	}
}

// TestRecordCarriesOnlyWhatThePrivacyModeKeeps runs the real ingest
// transform in front of the conversion, as the collector does.
func TestRecordCarriesOnlyWhatThePrivacyModeKeeps(t *testing.T) {
	apply := func(mode querylog.Mode, pseudonym bool) Record {
		t.Helper()
		p, err := querylog.NewPrivacy(querylog.PrivacyOptions{Mode: mode, Pseudonym: pseudonym})
		if err != nil {
			t.Fatal(err)
		}
		ev := fullEvent()
		if !p.Apply(ev) {
			t.Fatal("event excluded")
		}
		return From(ev)
	}
	full := apply(querylog.PrivacyFull, false)
	if full.Domain != "ads.example.com" || full.Client != "192.0.2.7" || full.Answer == "" {
		t.Errorf("full: %+v", full)
	}
	prefix := apply(querylog.PrivacyAnonymizeClient, false)
	if prefix.Client != "192.0.2.0" || prefix.ClientName != "" || prefix.ECS != "" || prefix.Answer != "" || prefix.Domain != "ads.example.com" {
		t.Errorf("anonymize_client (prefix): %+v", prefix)
	}
	pseudonym := apply(querylog.PrivacyAnonymizeClient, true)
	if pseudonym.Client != "" || len(pseudonym.ClientName) != len(querylog.PseudonymPrefix)+12 || pseudonym.ClientName[:5] != querylog.PseudonymPrefix {
		t.Errorf("anonymize_client (pseudonym): %+v", pseudonym)
	}
	hidden := apply(querylog.PrivacyHideDomains, false)
	if hidden.Domain != querylog.HiddenName || hidden.CNAME != querylog.HiddenName || hidden.Zone != querylog.HiddenName || hidden.Rule != "" || hidden.Answer != "" || hidden.Client != "192.0.2.7" {
		t.Errorf("hide_domains: %+v", hidden)
	}
	both := apply(querylog.PrivacyHideDomainsClients, false)
	if both.Domain != querylog.HiddenName || both.Client != "0.0.0.0" || both.ClientName != "" || both.ECS != "" {
		t.Errorf("hide_domains_clients: %+v", both)
	}
	// Nothing the mode hides survives anywhere in the encoded record.
	for name, tc := range map[string]struct {
		record Record
		hidden []string
	}{
		"anonymize_client (prefix)":    {prefix, []string{"192.0.2.7", "laptop", "198.51.100.0/24", "tracker.example.net."}},
		"anonymize_client (pseudonym)": {pseudonym, []string{"192.0.2", "laptop", "198.51.100.0/24"}},
		"hide_domains":                 {hidden, []string{"example.com", "example.net"}},
		"hide_domains_clients":         {both, []string{"example.com", "example.net", "192.0.2", "laptop", "198.51.100.0/24"}},
	} {
		raw, err := json.Marshal(tc.record)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range tc.hidden {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s: the record still carries %q: %s", name, leak, raw)
			}
		}
	}
}
