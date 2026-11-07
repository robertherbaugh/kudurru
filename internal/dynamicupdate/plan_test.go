package dynamicupdate

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func record(t testing.TB, text string) dns.RR {
	t.Helper()
	parts := strings.Fields(text)
	if len(parts) == 4 && (parts[2] == "ANY" || parts[2] == "NONE") {
		ttl, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		return &dns.RR_Header{Name: parts[0], Ttl: uint32(ttl), Class: dns.StringToClass[parts[2]], Rrtype: dns.StringToType[parts[3]]}
	}
	class := uint16(0)
	if len(parts) > 4 && (parts[2] == "ANY" || parts[2] == "NONE") {
		class = dns.StringToClass[parts[2]]
		parts[2] = "IN"
		text = strings.Join(parts, " ")
	}
	rr, err := dns.NewRR(text)
	if err != nil {
		t.Fatal(err)
	}
	if class != 0 {
		rr.Header().Class = class
	}
	return rr
}
func base(t testing.TB) State {
	return State{Apex: "example.", Records: []dns.RR{
		record(t, "example. 300 IN SOA ns.example. hostmaster.example. 4294967295 60 30 600 30"),
		record(t, "example. 300 IN NS ns.example."),
		record(t, "ns.example. 300 IN A 192.0.2.1"),
		record(t, "pair.example. 300 IN A 192.0.2.2"),
		record(t, "pair.example. 300 IN A 192.0.2.3"),
		record(t, "deep.ent.example. 300 IN TXT \"MiXeD\""),
	}, UpdateEnabled: true, AuthEpoch: 7,
		Credentials: []Credential{{Key: "update-key.", Version: "v1"}},
		Grants:      []Grant{{Key: "update-key.", Owner: "*.example."}, {Key: "update-key.", Owner: "example."}},
	}
}
func request(t testing.TB, prerequisites, mutations []string) *dns.Msg {
	t.Helper()
	msg := new(dns.Msg)
	msg.SetUpdate("example.")
	for _, text := range prerequisites {
		msg.Answer = append(msg.Answer, record(t, text))
	}
	for _, text := range mutations {
		msg.Ns = append(msg.Ns, record(t, text))
	}
	return msg
}
func plan(t testing.TB, s State, msg *dns.Msg) (*Result, *ProtocolError) {
	t.Helper()
	tx, e := Parse(msg, Limits{})
	if e != nil {
		return nil, e
	}
	return Plan(s, tx, Principal{Key: "UPDATE-KEY.", Version: "v1"}, time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC), Limits{})
}
func stringsOf(rrs []dns.RR) []string {
	out := make([]string, len(rrs))
	for i, rr := range rrs {
		out[i] = rr.String()
	}
	return out
}
func rrset(rrs []dns.RR, name string, typ uint16) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		n, _ := CanonicalName(rr.Header().Name)
		if n == name && rr.Header().Rrtype == typ {
			out = append(out, rr)
		}
	}
	return out
}

func TestPrerequisitesWholeRRsetAndNames(t *testing.T) {
	for _, tc := range []struct {
		name          string
		prerequisites []string
		code          int
	}{
		{"name exists", []string{"pair.example. 0 ANY ANY"}, 0},
		{"name absent", []string{"missing.example. 0 ANY ANY"}, dns.RcodeNameError},
		{"ENT does not exist", []string{"ent.example. 0 ANY ANY"}, dns.RcodeNameError},
		{"ENT not in use", []string{"ent.example. 0 NONE ANY"}, 0},
		{"name not in use fails", []string{"pair.example. 0 NONE ANY"}, dns.RcodeYXDomain},
		{"set exists", []string{"pair.example. 0 ANY A"}, 0},
		{"set absent", []string{"pair.example. 0 ANY AAAA"}, dns.RcodeNXRrset},
		{"set not in use", []string{"pair.example. 0 NONE AAAA"}, 0},
		{"set not in use fails", []string{"pair.example. 0 NONE A"}, dns.RcodeYXRrset},
		{"whole values unordered and duplicate", []string{"PAIR.EXAMPLE. 0 IN A 192.0.2.3", "pair.example. 0 IN A 192.0.2.2", "pair.example. 0 IN A 192.0.2.2"}, 0},
		{"subset is insufficient", []string{"pair.example. 0 IN A 192.0.2.2"}, dns.RcodeNXRrset},
		{"extra member fails", []string{"pair.example. 0 IN A 192.0.2.2", "pair.example. 0 IN A 192.0.2.3", "pair.example. 0 IN A 192.0.2.4"}, dns.RcodeNXRrset},
		{"TXT byte case matters", []string{"deep.ent.example. 0 IN TXT \"mixed\""}, dns.RcodeNXRrset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base(t)
			before := stringsOf(s.Records)
			result, e := plan(t, s, request(t, tc.prerequisites, []string{"new.example. 60 IN A 192.0.2.7"}))
			if tc.code != 0 {
				if e == nil || e.Rcode != tc.code || result != nil {
					t.Fatalf("result=%v error=%v want %d", result, e, tc.code)
				}
			} else if e != nil || !result.Changed {
				t.Fatalf("%v %v", result, e)
			}
			if !reflect.DeepEqual(before, stringsOf(s.Records)) {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestOrderedMutationsTTLAndSerialWrap(t *testing.T) {
	s := base(t)
	result, e := plan(t, s, request(t, nil, []string{
		"pair.example. 0 ANY A",
		"pair.example. 60 IN A 192.0.2.8",
		"pair.example. 0 NONE A 192.0.2.8",
		"pair.example. 80 IN A 192.0.2.9",
		"pair.example. 120 IN A 192.0.2.9",
		"unknown.example. 50 IN TYPE65200 \\# 4 AaBbCcDd",
	}))
	if e != nil {
		t.Fatal(e)
	}
	pair := rrset(result.Records, "pair.example.", dns.TypeA)
	if len(pair) != 1 || pair[0].Header().Ttl != 120 || pair[0].(*dns.A).A.String() != "192.0.2.9" || !result.Changed || result.Serial == 0 || !RFC1982Greater(result.Serial, 4294967295) {
		t.Fatalf("bad result: %+v %v", result, pair)
	}
	if len(rrset(result.Records, "unknown.example.", 65200)) != 1 || result.AuthEpoch != 7 || result.CredentialVersion != "v1" {
		t.Fatal("lost raw data/auth barrier")
	}
	// Candidate data is independent of input and transaction storage.
	result.Records[0].Header().Ttl = 999
	if s.Records[0].Header().Ttl == 999 {
		t.Fatal("candidate aliases input")
	}
}

func TestApexAndSingletonNoops(t *testing.T) {
	for _, tc := range []struct {
		name    string
		changes []string
		soa     string
		serial  uint32
	}{
		{"delete apex protects SOA NS", []string{"example. 0 ANY ANY"}, "", 4294967295},
		{"delete SOA set", []string{"example. 0 ANY SOA"}, "", 4294967295},
		{"delete final NS", []string{"example. 0 NONE NS ns.example."}, "", 4294967295},
		{"equal SOA ignored", nil, "example. 40 IN SOA other.example. other.example. 4294967295 1 1 1 1", 4294967295},
		{"lower SOA ignored", nil, "example. 40 IN SOA other.example. other.example. 4294967294 1 1 1 1", 4294967295},
		{"ambiguous SOA ignored", nil, "example. 40 IN SOA other.example. other.example. 2147483647 1 1 1 1", 4294967295},
		{"greater wrapping SOA", nil, "example. 40 IN SOA other.example. other.example. 4 1 1 1 1", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base(t)
			changes := append([]string(nil), tc.changes...)
			if tc.soa != "" {
				changes = append(changes, tc.soa)
			}
			r, e := plan(t, s, request(t, nil, changes))
			if e != nil {
				t.Fatal(e)
			}
			if r.Serial != tc.serial || len(rrset(r.Records, "example.", dns.TypeNS)) != 1 || len(rrset(r.Records, "example.", dns.TypeSOA)) != 1 {
				t.Fatalf("bad apex result %+v", r)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		changes   []string
		wantA     int
		wantCNAME int
	}{
		{"CNAME conflict ignored", []string{"pair.example. 60 IN CNAME ns.example.", "ok.example. 60 IN A 192.0.2.8"}, 2, 0},
		{"ordered CNAME replacement", []string{"pair.example. 0 ANY A", "pair.example. 60 IN CNAME ns.example.", "pair.example. 70 IN CNAME target.example.", "pair.example. 60 IN A 192.0.2.8"}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := plan(t, base(t), request(t, nil, tc.changes))
			if e != nil {
				t.Fatal(e)
			}
			if len(rrset(r.Records, "pair.example.", dns.TypeA)) != tc.wantA || len(rrset(r.Records, "pair.example.", dns.TypeCNAME)) != tc.wantCNAME {
				t.Fatalf("bad CNAME result %v", stringsOf(r.Records))
			}
		})
	}
}

func TestOwnershipAndCurrentAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*State)
		changes []string
	}{
		{"disabled", func(s *State) { s.UpdateEnabled = false }, []string{"ok.example. 60 IN A 192.0.2.8"}},
		{"rotation", func(s *State) { s.Credentials[0].Version = "v2" }, []string{"ok.example. 60 IN A 192.0.2.8"}},
		{"reserved inactive", func(s *State) {
			s.Reservations = []Reservation{{Name: "inactive.example.", Type: dns.TypeA, Source: "file:off"}}
		}, []string{"ok.example. 60 IN A 192.0.2.8", "inactive.example. 60 IN A 192.0.2.8"}},
		{"reserved deletion", func(s *State) {
			s.Reservations = []Reservation{{Name: "pair.example.", Type: dns.TypeA, Source: "api"}}
		}, []string{"pair.example. 0 ANY ANY"}},
		{"CNAME hides reservation", func(s *State) {
			s.Reservations = []Reservation{{Name: "inactive.example.", Type: dns.TypeA, Source: "api"}}
		}, []string{"inactive.example. 60 IN CNAME ns.example."}},
		{"DNAME subtree", func(s *State) {
			s.Reservations = []Reservation{{Name: "occluded.example.", Type: dns.TypeDNAME, Subtree: true, Source: "api"}}
		}, []string{"leaf.occluded.example. 60 IN A 192.0.2.8"}},
		{"narrow delete name", func(s *State) {
			s.Grants = []Grant{{Key: "update-key.", Owner: "*.example.", Types: []uint16{dns.TypeA}}}
			s.Records = append(s.Records, record(t, "pair.example. 40 IN TXT \"protected\""))
		}, []string{"pair.example. 0 ANY ANY"}},
		{"label boundary", func(s *State) { s.Grants = []Grant{{Key: "update-key.", Owner: "*.pair.example."}} }, []string{"notpair.example. 60 IN A 192.0.2.8"}},
		{"invalid grant", func(s *State) { s.Grants = []Grant{{Key: "update-key.", Owner: "bad*pattern.example."}} }, []string{"ok.example. 60 IN A 192.0.2.8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base(t)
			tc.edit(&s)
			before := stringsOf(s.Records)
			r, e := plan(t, s, request(t, nil, tc.changes))
			if e == nil || e.Rcode != dns.RcodeRefused || r != nil {
				t.Fatalf("%+v %v", r, e)
			}
			if !reflect.DeepEqual(before, stringsOf(s.Records)) {
				t.Fatal("denied packet modified input")
			}
		})
	}
}

func TestLiteralNamesCanonicalRDATAAndWKS(t *testing.T) {
	s := base(t)
	s.Records = append(s.Records, record(t, "alias.example. 100 IN CNAME NS.EXAMPLE."), record(t, "*.example. 100 IN TXT \"literal\""), record(t, "wks.example. 100 IN TYPE11 \\# 6 C00002010601"))
	r, e := plan(t, s, request(t, []string{"ALIAS.EXAMPLE. 0 IN CNAME ns.example.", "\\097lias.example. 0 ANY CNAME"}, []string{"*.example. 0 ANY TXT", "wks.example. 200 IN TYPE11 \\# 6 c000020106FF"}))
	if e != nil {
		t.Fatal(e)
	}
	if len(rrset(r.Records, "alias.example.", dns.TypeCNAME)) != 1 || len(rrset(r.Records, "ns.example.", dns.TypeA)) != 1 || len(rrset(r.Records, "*.example.", dns.TypeTXT)) != 0 || len(rrset(r.Records, "wks.example.", 11)) != 1 {
		t.Fatal("alias/wildcard/WKS semantics incorrect")
	}
}

func TestParseFramingAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*dns.Msg)
		code int
	}{
		{"zone count", func(m *dns.Msg) { m.Question = nil }, dns.RcodeFormatError},
		{"wrong class", func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS }, dns.RcodeNotAuth},
		{"outside", func(m *dns.Msg) { m.Ns = []dns.RR{record(t, "foreign. 60 IN A 192.0.2.2")} }, dns.RcodeNotZone},
		{"nonzero prerequisite ttl", func(m *dns.Msg) { m.Answer = []dns.RR{record(t, "pair.example. 1 ANY A")} }, dns.RcodeFormatError},
		{"nonempty meta RDATA", func(m *dns.Msg) { m.Answer = []dns.RR{record(t, "pair.example. 0 ANY A 192.0.2.1")} }, dns.RcodeFormatError},
		{"nonzero deletion ttl", func(m *dns.Msg) { m.Ns = []dns.RR{record(t, "pair.example. 1 NONE A 192.0.2.1")} }, dns.RcodeFormatError},
		{"pseudo addition", func(m *dns.Msg) {
			m.Ns = []dns.RR{&dns.RR_Header{Name: "pair.example.", Rrtype: dns.TypeAXFR, Class: dns.ClassINET}}
		}, dns.RcodeFormatError},
		{"nil RR", func(m *dns.Msg) { m.Ns = []dns.RR{nil} }, dns.RcodeFormatError},
		{"typed nil RR", func(m *dns.Msg) { var a *dns.A; m.Ns = []dns.RR{a} }, dns.RcodeFormatError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := request(t, nil, nil)
			tc.edit(m)
			_, e := Parse(m, Limits{})
			if e == nil || e.Rcode != tc.code {
				t.Fatalf("%v", e)
			}
		})
	}
	msg := request(t, nil, []string{"ok.example. 60 IN A 192.0.2.8"})
	if _, e := Parse(msg, Limits{MaxUpdateBytes: 20}); e == nil {
		t.Fatal("byte bound bypassed")
	}
	msg.Ns = append(msg.Ns, record(t, "other.example. 60 IN A 192.0.2.8"))
	if _, e := Parse(msg, Limits{MaxUpdateRecords: 1}); e == nil {
		t.Fatal("record bound bypassed")
	}
	tx, e := Parse(msg, Limits{})
	if e != nil {
		t.Fatal(e)
	}
	if r, e := Plan(base(t), tx, Principal{Key: "update-key.", Version: "v1"}, time.Now(), Limits{MaxRecords: 6}); e == nil || e.Rcode != dns.RcodeServerFailure || r != nil {
		t.Fatal("candidate capacity not enforced")
	}
}
