package authoritative

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

const (
	soaOrg     = "example.org. 300 IN SOA ns1.example.org. hostmaster.example.org. 2026101100 3600 900 1209600 300"
	soaNet     = "example.net. 300 IN SOA ns1.example.net. hostmaster.example.net. 2026101100 3600 900 1209600 300"
	upstreamA  = "203.0.113.7"
	lanClient  = "192.168.1.10"
	upstreamID = "default/192.0.2.53:53"
)

func ep(name, typ string, targets ...string) v1alpha1.Endpoint {
	return v1alpha1.Endpoint{DNSName: name, RecordType: typ, Targets: targets}
}

func dnsZone(name string, mutate ...func(*v1alpha1.DNSZone)) *v1alpha1.DNSZone {
	dz := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Trim(name, "."), Generation: 1},
		Spec: v1alpha1.DNSZoneSpec{
			ZoneName:    name,
			Type:        v1alpha1.ZoneTypePrimary,
			Nameservers: []string{"ns1." + name},
			DefaultTTL:  300,
		},
		Status: v1alpha1.DNSZoneStatus{Serial: 2026101100},
	}
	for _, m := range mutate {
		m(dz)
	}
	return dz
}

func build(t testing.TB, dz *v1alpha1.DNSZone, eps ...v1alpha1.Endpoint) *zone.Zone {
	t.Helper()
	var sets []*v1alpha1.DNSRecordSet
	if len(eps) > 0 {
		sets = append(sets, &v1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "dns", Name: "records"},
			Spec:       v1alpha1.DNSRecordSetSpec{Endpoints: eps},
		})
	}
	z, errs := zone.Build(dz, sets, zone.BuildOptions{})
	if !errs.Empty() {
		t.Fatalf("build %s: zone=%v sets=%v", dz.Spec.ZoneName, errs.Zone, errs.RecordSets)
	}
	return z
}

// testZones is the fixture every golden case runs against.
func testZones(t testing.TB) []*zone.Zone {
	org := build(t, dnsZone("example.org."),
		ep("ns1.example.org", "A", "192.0.2.1"),
		ep("www.example.org", "CNAME", "web.example.org"),
		ep("web.example.org", "A", "192.0.2.80"),
		ep("example.org", "MX", "10 mail.example.org"),
		ep("mail.example.org", "A", "192.0.2.25"),
		ep("example.org", "TXT", "v=spf1 -all"),
		ep("*.wild.example.org", "A", "192.0.2.99"),
		ep("sub.example.org", "NS", "ns.sub.example.org"),
		ep("ns.sub.example.org", "A", "192.0.2.53"),
		ep("cross.example.org", "CNAME", "host.example.net"),
		ep("dangling.example.org", "CNAME", "nohost.example.net"),
		ep("ext.example.org", "CNAME", "www.external.test"),
		ep("to-corp.example.org", "CNAME", "app.corp.example"),
		ep("to-private.example.org", "CNAME", "host.private.example"),
		ep("loop1.example.org", "CNAME", "loop2.example.org"),
		ep("loop2.example.org", "CNAME", "loop1.example.org"),
		ep("bounce.example.org", "CNAME", "back.example.net"),
		ep("dn.example.org", "DNAME", "example.net"),
	)
	net := build(t, dnsZone("example.net."),
		ep("host.example.net", "A", "198.51.100.1"),
		ep("back.example.net", "CNAME", "web.example.org"),
	)
	corp := build(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "corp"}
	}))
	private := build(t, dnsZone("private.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Policy = v1alpha1.ZonePolicy{
			QueryAccess: v1alpha1.AccessUseNetworkACL,
			QueryACL:    []string{"!10.0.0.66", "10.0.0.0/8", "2001:db8:a::/48"},
		}
	}), ep("host.private.example", "A", "10.0.0.5"))
	return []*zone.Zone{org, net, corp, private}
}

func source(t testing.TB, synced bool) *zone.Holder {
	t.Helper()
	snap, err := zone.NewSnapshot(1, synced, testZones(t)...)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	return h
}

// recorder is the rest of the pipeline: it answers A questions with one
// record (NXDOMAIN for names under nx.test.) and records what it saw.
type recorder struct {
	calls []*pipeline.Request
	err   error
}

func (r *recorder) next(req *pipeline.Request) (*pipeline.Response, error) {
	r.calls = append(r.calls, req)
	if r.err != nil {
		return nil, r.err
	}
	q := req.Msg.Question[0]
	m := new(dns.Msg)
	m.SetReply(req.Msg)
	m.RecursionAvailable = true
	switch {
	case strings.HasSuffix(q.Name, "nx.test."):
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{mustRR("test. 900 IN SOA a.test. b.test. 1 2 3 4 5")}
	case q.Qtype == dns.TypeA:
		m.Answer = []dns.RR{mustRR(q.Name + " 60 IN A " + upstreamA)}
	}
	return &pipeline.Response{Msg: m, Status: pipeline.StatusForwarded, Upstream: upstreamID, Cacheable: true}, nil
}

func mustRR(s string) dns.RR {
	r, err := dns.NewRR(s)
	if err != nil {
		panic(err)
	}
	return r
}

// Case is one golden exchange in the style of CoreDNS plugin/test.Case: a
// question, the client context, and the exact reply expected.
type Case struct {
	Name   string
	Qname  string
	Qtype  uint16
	Opcode int
	NoRD   bool
	Client string // default lanClient
	Proto  pipeline.Proto
	EDNS   bool

	Rcode    int
	AA       bool
	Status   pipeline.Status
	Upstream string
	Answer   []string
	Ns       []string
	Extra    []string

	// Next is the question the rest of the pipeline must see ("" = next
	// not called) and Group the upstream-group hint it must carry.
	Next  string
	Group string
}

func (tc *Case) request() *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(tc.Qname, tc.Qtype)
	m.Opcode = tc.Opcode
	m.RecursionDesired = !tc.NoRD
	if tc.EDNS {
		m.SetEdns0(4096, true)
	}
	client := tc.Client
	if client == "" {
		client = lanClient
	}
	return &pipeline.Request{
		Msg:    m,
		Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(client), 40000), Proto: tc.Proto},
	}
}

func sorted(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, r := range rrs {
		if r.Header().Rrtype == dns.TypeOPT {
			continue
		}
		out = append(out, r.String())
	}
	slices.Sort(out)
	return out
}

func normalised(t *testing.T, ss []string) []string {
	t.Helper()
	out := make([]string, len(ss))
	for i, s := range ss {
		r, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("bad expectation %q: %v", s, err)
		}
		out[i] = r.String()
	}
	slices.Sort(out)
	return out
}

func run(t *testing.T, st *Stage, cases []Case) {
	t.Helper()
	for _, tc := range cases {
		name := tc.Name
		if name == "" {
			name = tc.Qname + "/" + dns.TypeToString[tc.Qtype]
		}
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			req := tc.request()
			resp, err := st.Serve(req, rec.next)
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
			m := resp.Msg
			if m.Id != req.Msg.Id || !m.Response {
				t.Errorf("reply id/QR: %d %v", m.Id, m.Response)
			}
			if m.Rcode != tc.Rcode {
				t.Errorf("rcode = %s, want %s", dns.RcodeToString[m.Rcode], dns.RcodeToString[tc.Rcode])
			}
			if m.Authoritative != tc.AA {
				t.Errorf("AA = %v, want %v", m.Authoritative, tc.AA)
			}
			if resp.Status != tc.Status {
				t.Errorf("status = %s, want %s", resp.Status, tc.Status)
			}
			if tc.Upstream != "" && resp.Upstream != tc.Upstream {
				t.Errorf("upstream = %q, want %q", resp.Upstream, tc.Upstream)
			}
			for _, sec := range []struct {
				name      string
				got, want []string
			}{
				{"answer", sorted(m.Answer), normalised(t, tc.Answer)},
				{"authority", sorted(m.Ns), normalised(t, tc.Ns)},
				{"additional", sorted(m.Extra), normalised(t, tc.Extra)},
			} {
				if !slices.Equal(sec.got, sec.want) {
					t.Errorf("%s section:\n got: %q\nwant: %q", sec.name, sec.got, sec.want)
				}
			}
			if tc.EDNS != (m.IsEdns0() != nil) {
				t.Errorf("OPT present = %v, want %v", m.IsEdns0() != nil, tc.EDNS)
			}
			switch {
			case tc.Next == "" && len(rec.calls) > 0:
				t.Errorf("next called with %v", rec.calls[0].Msg.Question)
			case tc.Next != "" && len(rec.calls) != 1:
				t.Errorf("next called %d times, want once for %s", len(rec.calls), tc.Next)
			case tc.Next != "":
				sub := rec.calls[0]
				if got := sub.Msg.Question[0].Name; got != tc.Next {
					t.Errorf("next asked for %s, want %s", got, tc.Next)
				}
				if sub.UpstreamGroup != tc.Group {
					t.Errorf("upstream group hint = %q, want %q", sub.UpstreamGroup, tc.Group)
				}
			}
		})
	}
}

func TestServeGolden(t *testing.T) {
	st := New(source(t, true))
	ns := []string{"example.org. 300 IN NS ns1.example.org."}
	glue := []string{"ns1.example.org. 300 IN A 192.0.2.1"}
	run(t, st, []Case{
		// Success: AA, glue in additional.
		{Qname: "web.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative, Upstream: "zone:example.org.",
			Answer: []string{"web.example.org. 300 IN A 192.0.2.80"}},
		{Qname: "example.org.", Qtype: dns.TypeNS, AA: true, Status: pipeline.StatusAuthoritative, Answer: ns, Extra: glue},
		{Qname: "example.org.", Qtype: dns.TypeMX, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"example.org. 300 IN MX 10 mail.example.org."}, Extra: []string{"mail.example.org. 300 IN A 192.0.2.25"}},
		{Name: "edns-echo", Qname: "web.example.org.", Qtype: dns.TypeA, EDNS: true, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"web.example.org. 300 IN A 192.0.2.80"}},

		// NODATA and NXDOMAIN: SOA in authority, never forwarded.
		{Name: "nodata", Qname: "web.example.org.", Qtype: dns.TypeAAAA, AA: true, Status: pipeline.StatusAuthoritative, Ns: []string{soaOrg}},
		{Name: "nxdomain", Qname: "nope.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeNameError, AA: true, Status: pipeline.StatusAuthoritative, Ns: []string{soaOrg}},

		// Delegation: referral, AA clear.
		{Name: "delegation", Qname: "www.sub.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusAuthoritative,
			Ns: []string{"sub.example.org. 300 IN NS ns.sub.example.org."}, Extra: []string{"ns.sub.example.org. 300 IN A 192.0.2.53"}},

		// Wildcard synthesis.
		{Name: "wildcard", Qname: "a.b.wild.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"a.b.wild.example.org. 300 IN A 192.0.2.99"}},

		// CNAME chains inside the zone, into another local zone, and back.
		{Name: "cname-in-zone", Qname: "www.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"www.example.org. 300 IN CNAME web.example.org.", "web.example.org. 300 IN A 192.0.2.80"}},
		{Name: "cname-cross-zone", Qname: "cross.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative, Upstream: "zone:example.net.",
			Answer: []string{"cross.example.org. 300 IN CNAME host.example.net.", "host.example.net. 300 IN A 198.51.100.1"}},
		{Name: "cname-cross-zone-nxdomain", Qname: "dangling.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeNameError, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"dangling.example.org. 300 IN CNAME nohost.example.net."}, Ns: []string{soaNet}},
		{Name: "cname-there-and-back", Qname: "bounce.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"bounce.example.org. 300 IN CNAME back.example.net.", "back.example.net. 300 IN CNAME web.example.org.", "web.example.org. 300 IN A 192.0.2.80"}},
		{Name: "cname-loop", Qname: "loop1.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeServerFailure, Status: pipeline.StatusAuthoritative},

		// A chain that leaves every local zone: RD splices the rest of the
		// pipeline's answer; no RD returns the partial chain.
		{Name: "cname-leaves-rd", Qname: "ext.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Upstream: upstreamID,
			Answer: []string{"ext.example.org. 300 IN CNAME www.external.test.", "www.external.test. 60 IN A " + upstreamA},
			Next:   "www.external.test."},
		{Name: "cname-leaves-no-rd", Qname: "ext.example.org.", Qtype: dns.TypeA, NoRD: true, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"ext.example.org. 300 IN CNAME www.external.test."}},
		{Name: "cname-into-forward-zone", Qname: "to-corp.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded,
			Answer: []string{"to-corp.example.org. 300 IN CNAME app.corp.example.", "app.corp.example. 60 IN A " + upstreamA},
			Next:   "app.corp.example.", Group: "corp"},
		{Name: "cname-into-refused-zone", Qname: "to-private.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"to-private.example.org. 300 IN CNAME host.private.example."}},

		// DNAME: synthesised CNAME, then the target zone.
		{Name: "dname", Qname: "host.dn.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"dn.example.org. 300 IN DNAME example.net.", "host.dn.example.org. 300 IN CNAME host.example.net.", "host.example.net. 300 IN A 198.51.100.1"}},
		{Name: "dname-nxdomain-in-target", Qname: "nohost.dn.example.org.", Qtype: dns.TypeA, Rcode: dns.RcodeNameError, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"dn.example.org. 300 IN DNAME example.net.", "nohost.dn.example.org. 300 IN CNAME nohost.example.net."},
			Ns:     []string{soaNet}},

		// Forward zone: hint and next.
		{Name: "forward-zone", Qname: "app.corp.example.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded, Upstream: upstreamID,
			Answer: []string{"app.corp.example. 60 IN A " + upstreamA}, Next: "app.corp.example.", Group: "corp"},

		// Not ours: next, untouched.
		{Name: "no-zone", Qname: "www.external.test.", Qtype: dns.TypeA, Status: pipeline.StatusForwarded,
			Answer: []string{"www.external.test. 60 IN A " + upstreamA}, Next: "www.external.test."},

		// Opcodes and transfers.
		{Name: "notify", Qname: "example.org.", Qtype: dns.TypeSOA, Opcode: dns.OpcodeNotify, Rcode: dns.RcodeNotImplemented, Status: pipeline.StatusAuthoritative},
		{Name: "update", Qname: "example.org.", Qtype: dns.TypeSOA, Opcode: dns.OpcodeUpdate, Rcode: dns.RcodeNotImplemented, Status: pipeline.StatusAuthoritative},
		// Transfers: zoneTransfer defaults to deny (transfer_test.go has
		// the policy matrix).
		{Name: "axfr", Qname: "example.org.", Qtype: dns.TypeAXFR, Proto: pipeline.ProtoTCP, Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL, Upstream: "zone:example.org."},
		{Name: "ixfr", Qname: "example.org.", Qtype: dns.TypeIXFR, Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL, Upstream: "zone:example.org."},

		// RFC 8482: ANY over UDP is minimal, over TCP complete.
		{Name: "any-udp", Qname: "example.org.", Qtype: dns.TypeANY, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"example.org. 300 IN NS ns1.example.org."}},
		{Name: "any-tcp", Qname: "example.org.", Qtype: dns.TypeANY, Proto: pipeline.ProtoTCP, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"example.org. 300 IN NS ns1.example.org.", soaOrg, "example.org. 300 IN MX 10 mail.example.org.", `example.org. 300 IN TXT "v=spf1 -all"`}},
	})
}

func TestServeQueryACL(t *testing.T) {
	st := New(source(t, true))
	ok := []string{"host.private.example. 300 IN A 10.0.0.5"}
	run(t, st, []Case{
		{Name: "allowed", Qname: "host.private.example.", Qtype: dns.TypeA, Client: "10.1.2.3", AA: true, Status: pipeline.StatusAuthoritative, Answer: ok},
		{Name: "allowed-v6", Qname: "host.private.example.", Qtype: dns.TypeA, Client: "2001:db8:a::1", AA: true, Status: pipeline.StatusAuthoritative, Answer: ok},
		{Name: "allowed-v4-mapped", Qname: "host.private.example.", Qtype: dns.TypeA, Client: "::ffff:10.1.2.3", AA: true, Status: pipeline.StatusAuthoritative, Answer: ok},
		{Name: "negated-first-match", Qname: "host.private.example.", Qtype: dns.TypeA, Client: "10.0.0.66", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL, Upstream: "zone:private.example."},
		{Name: "default-deny", Qname: "host.private.example.", Qtype: dns.TypeA, Client: "192.168.1.10", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
		{Name: "denial-not-leaked", Qname: "nope.private.example.", Qtype: dns.TypeA, Client: "192.168.1.10", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
		{Name: "refused-before-axfr", Qname: "private.example.", Qtype: dns.TypeAXFR, Client: "192.168.1.10", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
	})
}

func TestServeForwardZoneACL(t *testing.T) {
	fwd := build(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Policy.QueryAccess = v1alpha1.AccessAllowOnlyPrivate
	}))
	snap, err := zone.NewSnapshot(1, true, fwd)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	run(t, New(h), []Case{
		{Name: "private-client", Qname: "a.corp.example.", Qtype: dns.TypeA, Client: "10.0.0.1", Status: pipeline.StatusForwarded,
			Answer: []string{"a.corp.example. 60 IN A " + upstreamA}, Next: "a.corp.example.", Group: zone.DefaultUpstreamGroup},
		{Name: "public-client", Qname: "a.corp.example.", Qtype: dns.TypeA, Client: "198.51.100.9", Rcode: dns.RcodeRefused, Status: pipeline.StatusRefusedACL},
	})
}

func TestServeUnsynced(t *testing.T) {
	servfail := func(name, qname string, qtype uint16) Case {
		return Case{Name: name, Qname: qname, Qtype: qtype, Rcode: dns.RcodeServerFailure, Status: pipeline.StatusServfailUnsynced, Upstream: UpstreamUnsynced}
	}
	run(t, New(source(t, false)), []Case{
		// Positive data the zone asserts is still served.
		{Name: "positive-served", Qname: "web.example.org.", Qtype: dns.TypeA, AA: true, Status: pipeline.StatusAuthoritative,
			Answer: []string{"web.example.org. 300 IN A 192.0.2.80"}},
		{Name: "referral-served", Qname: "www.sub.example.org.", Qtype: dns.TypeA, Status: pipeline.StatusAuthoritative,
			Ns: []string{"sub.example.org. 300 IN NS ns.sub.example.org."}, Extra: []string{"ns.sub.example.org. 300 IN A 192.0.2.53"}},
		// Denials, chains that leave, unknown names and forward zones are
		// not trustworthy yet: SERVFAIL, never NXDOMAIN, never forwarded.
		servfail("nxdomain", "nope.example.org.", dns.TypeA),
		servfail("nodata", "web.example.org.", dns.TypeAAAA),
		servfail("cname-leaves", "ext.example.org.", dns.TypeA),
		servfail("no-zone", "www.external.test.", dns.TypeA),
		servfail("forward-zone", "app.corp.example.", dns.TypeA),
	})
	for name, src := range map[string]Source{"nil-source": nil, "empty-holder": new(zone.Holder)} {
		st := New(src)
		if st.Ready() {
			t.Errorf("%s: Ready before the first snapshot", name)
		}
		run(t, st, []Case{servfail(name, "web.example.org.", dns.TypeA)})
	}
	if !New(source(t, true)).Ready() || New(source(t, false)).Ready() {
		t.Error("Ready must follow Synced")
	}
}

func TestServePassesThrough(t *testing.T) {
	st := New(source(t, true))
	for name, mutate := range map[string]func(*dns.Msg){
		"chaos-class": func(m *dns.Msg) { m.Question[0].Qclass = dns.ClassCHAOS },
		"no-question": func(m *dns.Msg) { m.Question = nil },
		"two-questions": func(m *dns.Msg) {
			m.Question = append(m.Question, dns.Question{Name: "www.example.org.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
		},
		"status-opcode": func(m *dns.Msg) { m.Opcode = dns.OpcodeStatus },
	} {
		t.Run(name, func(t *testing.T) {
			tc := Case{Qname: "web.example.org.", Qtype: dns.TypeA}
			req := tc.request()
			mutate(req.Msg)
			called := false
			want := &pipeline.Response{}
			got, err := st.Serve(req, func(*pipeline.Request) (*pipeline.Response, error) { called = true; return want, nil })
			if err != nil || got != want || !called {
				t.Fatalf("must pass to next untouched: %v %v %v", got, err, called)
			}
		})
	}
}

func TestServeSetsRequestZone(t *testing.T) {
	st := New(source(t, true))
	tc := Case{Qname: "web.example.org.", Qtype: dns.TypeA}
	req := tc.request()
	if _, err := st.Serve(req, (&recorder{}).next); err != nil {
		t.Fatal(err)
	}
	if z, ok := req.Zone.(*zone.Zone); !ok || z.Name() != "example.org." {
		t.Fatalf("req.Zone = %#v", req.Zone)
	}
}

func TestSpliceFailures(t *testing.T) {
	st := New(source(t, true))
	tc := Case{Qname: "ext.example.org.", Qtype: dns.TypeA}
	resp, err := st.Serve(tc.request(), (&recorder{err: errors.New("boom")}).next)
	if err != nil {
		t.Fatal(err)
	}
	if m := resp.Msg; m.Rcode != dns.RcodeServerFailure || resp.Status != pipeline.StatusServfailUpstream || len(m.Answer) != 1 {
		t.Fatalf("upstream error keeps the chain and answers SERVFAIL: %v", m)
	}

	// A negative answer for the target keeps the chain and takes the
	// target's rcode and authority (RFC 6604).
	h := source(t, true)
	org := build(t, dnsZone("example.org."), ep("gone.example.org", "CNAME", "x.nx.test"))
	h.Store(h.Load().With(2, org))
	tc = Case{Qname: "gone.example.org.", Qtype: dns.TypeA}
	resp, err = New(h).Serve(tc.request(), (&recorder{}).next)
	if err != nil {
		t.Fatal(err)
	}
	if m := resp.Msg; m.Rcode != dns.RcodeNameError || len(m.Answer) != 1 || len(m.Ns) != 1 || m.Authoritative {
		t.Fatalf("spliced NXDOMAIN: %v", m)
	}
}

func TestNotifyUpdateHandlers(t *testing.T) {
	st := New(source(t, true))
	var got []int
	h := func(req *pipeline.Request) (*pipeline.Response, error) {
		got = append(got, req.Msg.Opcode)
		return pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusAuthoritative), nil
	}
	st.Notify, st.Update = h, h
	for _, op := range []int{dns.OpcodeNotify, dns.OpcodeUpdate} {
		tc := Case{Qname: "example.org.", Qtype: dns.TypeSOA, Opcode: op}
		resp, err := st.Serve(tc.request(), (&recorder{}).next)
		if err != nil || resp.Msg.Rcode != dns.RcodeSuccess {
			t.Fatalf("opcode %d: %v %v", op, resp, err)
		}
	}
	if !slices.Equal(got, []int{dns.OpcodeNotify, dns.OpcodeUpdate}) {
		t.Fatalf("handlers saw %v", got)
	}
}

func TestTransferSlot(t *testing.T) {
	// Without a synced zone set the capability hands out nothing.
	var tp TransferProvider = New(nil)
	if ch, err := tp.Transfer("example.org.", 0); ch != nil || !errors.Is(err, ErrUnsynced) {
		t.Fatalf("Transfer: %v %v", ch, err)
	}
}

// upstreamStage stands in for stages/upstream in the chain test: it
// records the group hint the authoritative stage left on the request.
type upstreamStage struct{ groups []string }

func (u *upstreamStage) Name() string { return "upstream" }

func (u *upstreamStage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	u.groups = append(u.groups, req.UpstreamGroup)
	return (&recorder{}).next(req)
}

func TestInChain(t *testing.T) {
	up := &upstreamStage{}
	chain := pipeline.MustNew(New(source(t, true)), up)
	for _, q := range []string{"web.example.org.", "app.corp.example.", "www.external.test.", "ext.example.org."} {
		tc := Case{Qname: q, Qtype: dns.TypeA}
		resp, err := chain.Serve(tc.request())
		if err != nil || resp.Msg.Rcode != dns.RcodeSuccess || len(resp.Msg.Answer) == 0 {
			t.Fatalf("%s: %v %v", q, resp, err)
		}
	}
	if want := []string{"corp", "", ""}; !slices.Equal(up.groups, want) {
		t.Fatalf("upstream saw groups %q, want %q", up.groups, want)
	}
}
