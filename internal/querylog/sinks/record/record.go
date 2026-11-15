// Package record is the flat JSON form of a query event that the HTTP batch
// sinks export (D371): one ClickHouse row in JSONEachRow, and the log line
// of one Loki entry. The field names are a contract with the table
// definition and the queries documented in docs/deploy/query-log-sinks.md;
// add fields, never rename or retype one.
package record

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// TimeLayout is RFC 3339 in UTC with microseconds, the precision the local
// query log stores.
const TimeLayout = "2006-01-02T15:04:05.000000Z"

// Record is one query. Events arrive after the privacy transform, so a
// record holds exactly what the local query log keeps: a hidden name is
// "hidden", a hidden client 0.0.0.0, a pseudonymized client has no address.
// Empty optional fields are left out; a ClickHouse column then takes its
// default.
type Record struct {
	TS   string `json:"ts"`
	ID   uint64 `json:"id"`
	Node string `json:"node"`
	// Proto is udp, tcp, dot, doh, doq or doh3.
	Proto       string `json:"proto"`
	Client      string `json:"client,omitempty"`
	ClientName  string `json:"client_name,omitempty"`
	ClientGroup string `json:"client_group,omitempty"`
	// Domain is the query name in lower case without the trailing dot;
	// "." for the root.
	Domain string `json:"domain"`
	// QType and Rcode are mnemonics (A, HTTPS, TYPE65280; NOERROR, NXDOMAIN).
	QType  string `json:"qtype"`
	QClass uint16 `json:"qclass"`
	// Status is the query-log status (forwarded, cached, blocked_list, ...)
	// and StatusClass its group: permitted, cached, blocked or error.
	Status      string `json:"status"`
	StatusClass string `json:"status_class"`
	Rcode       string `json:"rcode"`
	ReplyType   string `json:"reply_type"`
	Answers     int    `json:"answers"`
	// Answer is the answer summary; present only under privacy full.
	Answer    string `json:"answer,omitempty"`
	LatencyUS int64  `json:"latency_us"`
	Upstream  string `json:"upstream,omitempty"`
	Zone      string `json:"zone,omitempty"`
	List      string `json:"list,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Policy    string `json:"policy,omitempty"`
	CNAME     string `json:"cname,omitempty"`
	DNSSEC    string `json:"dnssec"`
	// EDE is the extended DNS error code, when the answer carried one.
	EDE *uint16 `json:"ede,omitempty"`
	ECS string  `json:"ecs,omitempty"`
	// TraceID is the W3C trace ID in hex, when the query was traced.
	TraceID string `json:"trace_id,omitempty"`
	// Flags lists the set flags of rd, do, tc, ad, cd, cached, stale,
	// prefetch, ecs and traceparent.
	Flags []string `json:"flags,omitempty"`
}

// From converts an event. It reads the event only.
func From(ev *querylog.Event) Record {
	r := Record{
		TS: ev.Time.UTC().Format(TimeLayout), ID: ev.ID, Node: ev.Node, Proto: ev.Proto.String(),
		ClientName: ev.ClientName, ClientGroup: ev.ClientGroup, Domain: Name(ev.Name),
		QType: querylog.QTypeName(ev.QType), QClass: ev.QClass,
		Status: ev.Status.String(), StatusClass: string(ev.Status.Class()), Rcode: querylog.RcodeName(ev.Rcode),
		ReplyType: ev.ReplyType.String(), Answers: ev.Answers, Answer: ev.Answer, LatencyUS: ev.Latency.Microseconds(),
		Upstream: ev.Upstream, List: ev.List, Rule: ev.Rule, Policy: ev.Policy, DNSSEC: ev.DNSSEC.String(),
		Flags: flags(ev.Flags),
	}
	if r.QClass == 0 {
		r.QClass = 1 // IN, as the local query log stores it
	}
	if ev.Client.IsValid() {
		r.Client = ev.Client.String()
	}
	if ev.Zone != "" {
		r.Zone = Name(ev.Zone)
	}
	if ev.CNAME != "" {
		r.CNAME = Name(ev.CNAME)
	}
	if ev.HasEDE {
		code := ev.EDE
		r.EDE = &code
	}
	if ev.ECS.IsValid() {
		r.ECS = ev.ECS.String()
	}
	if ev.Traced() {
		r.TraceID = hex.EncodeToString(ev.TraceID[:])
	}
	return r
}

// Name is the form names take in the query log: lower case, no trailing
// dot, "." for the root.
func Name(name string) string {
	if name == "." || name == "" {
		return "."
	}
	return strings.TrimSuffix(strings.ToLower(name), ".")
}

// Time parses a TS value.
func Time(ts string) (time.Time, error) { return time.Parse(TimeLayout, ts) }

func flags(f querylog.Flags) []string {
	var out []string
	for _, x := range []struct {
		on   bool
		name string
	}{
		{f.RD, "rd"}, {f.DO, "do"}, {f.TC, "tc"}, {f.AD, "ad"}, {f.CD, "cd"},
		{f.Cached, "cached"}, {f.Stale, "stale"}, {f.Prefetch, "prefetch"},
		{f.ECS, "ecs"}, {f.Traceparent, "traceparent"},
	} {
		if x.on {
			out = append(out, x.name)
		}
	}
	return out
}
