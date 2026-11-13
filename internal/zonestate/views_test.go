package zonestate

import (
	"encoding/json"
	"testing"

	"github.com/miekg/dns"
)

func viewFixture(t *testing.T) *Aggregate {
	t.Helper()
	a := fixture(t)
	a.Sources = []Source{{ID: "DNSRecordSet/dns/split", UID: "u", ResourceVersion: "1", Manager: "api", Desired: json.RawMessage(`{"state":"present"}`)}}
	a.Views = []ViewRecords{
		{Group: "dns/lan", Records: []Record{viewRecord(t, "www.example. 60 IN A 10.0.0.2", "DNSRecordSet/dns/split"), viewRecord(t, "www.example. 60 IN A 10.0.0.1", "DNSRecordSet/dns/split")}},
		{Group: "dns/kids", Records: []Record{viewRecord(t, "www.example. 60 IN A 10.9.0.1", "DNSRecordSet/dns/split")}},
	}
	return a
}

func viewRecord(t *testing.T, text, source string) Record {
	t.Helper()
	rr, err := dns.NewRR(text)
	if err != nil {
		t.Fatal(err)
	}
	r, err := FromRR(rr, Provenance{SourceID: source})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Client-group views (D403) survive the codec in canonical order, count in
// the content hash, and never accept data a view cannot hold.
func TestCodecClientGroupViews(t *testing.T) {
	a := viewFixture(t)
	p, err := Prepare(a, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := readPrepared(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Views) != 2 || got.Views[0].Group != "dns/kids" || got.Views[1].Group != "dns/lan" || len(got.Views[1].Records) != 2 {
		t.Fatalf("decoded views %+v", got.Views)
	}
	rrs, err := got.ViewRRs()
	if err != nil || rrs[1][0].(*dns.A).A.String() != "10.0.0.1" {
		t.Fatalf("view records not in canonical order: %v %v", rrs, err)
	}
	plain := fixture(t)
	plain.Sources = a.Sources
	q, err := Prepare(plain, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if q.Head.Spec.ContentHash == p.Head.Spec.ContentHash {
		t.Fatal("views must count in the content hash")
	}
	// The default records are unchanged by views.
	if len(got.Records) != len(plain.Records) {
		t.Fatal("views leaked into the default records")
	}
	for name, mutate := range map[string]func(*Aggregate){
		"dynamic":   func(a *Aggregate) { a.Views[0].Records[0].Provenance = Provenance{Key: "key.", CredentialVersion: "v1"} },
		"system":    func(a *Aggregate) { a.Views[0].Records[0].Provenance = Provenance{SourceID: "system"} },
		"unknown":   func(a *Aggregate) { a.Views[0].Records[0].Provenance = Provenance{SourceID: "DNSRecordSet/dns/other"} },
		"ns":        func(a *Aggregate) { a.Views[0].Records[0] = viewRecord(t, "sub.example. 60 IN NS ns.example.net.", "DNSRecordSet/dns/split") },
		"soa":       func(a *Aggregate) { a.Views[0].Records[0] = viewRecord(t, "example. 60 IN SOA a. b. 1 2 3 4 5", "DNSRecordSet/dns/split") },
		"outside":   func(a *Aggregate) { a.Views[0].Records[0] = viewRecord(t, "www.example.net. 60 IN A 10.0.0.1", "DNSRecordSet/dns/split") },
		"duplicate": func(a *Aggregate) { a.Views[0].Records[1] = a.Views[0].Records[0] },
		"group":     func(a *Aggregate) { a.Views[1].Group = "dns/lan" },
		"empty":     func(a *Aggregate) { a.Views[1].Records = nil },
	} {
		bad := viewFixture(t)
		mutate(bad)
		if _, err := Prepare(bad, nil, Limits{}); err == nil {
			t.Errorf("%s: invalid view accepted", name)
		}
	}
	// Removing a source's fragment removes its view records with it.
	out, err := ReplaceSource(a, Projection{Source: a.Sources[0], Removed: true}, testNow(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Views) != 0 {
		t.Fatalf("removed source kept view records: %+v", out.Views)
	}
}
