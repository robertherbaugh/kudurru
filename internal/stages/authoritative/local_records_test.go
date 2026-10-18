package authoritative

import (
	"net/netip"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type testLocalRecords struct {
	calls   int
	handled bool
}

func (l *testLocalRecords) ResolveLocal(req *pipeline.Request) (*pipeline.Response, bool) {
	l.calls++
	if !l.handled {
		return nil, false
	}
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
	r.Msg.Authoritative = true
	r.Msg.Answer = []dns.RR{mustRR(req.Msg.Question[0].Name + " 30 IN A 192.0.2.10")}
	return r, true
}

func TestLocalRecordsZonePrecedence(t *testing.T) {
	root := func(forward, deny bool) *zone.Zone {
		return build(t, dnsZone(".", func(z *v1alpha1.DNSZone) {
			z.Spec.Nameservers = []string{"ns.example."}
			if forward {
				z.Spec.Type = v1alpha1.ZoneTypeForward
				z.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "root"}
			}
			if deny {
				z.Spec.Policy = v1alpha1.ZonePolicy{QueryAccess: v1alpha1.AccessUseNetworkACL, QueryACL: []string{"10.0.0.0/8"}}
			}
		}))
	}
	forward := build(t, dnsZone("leases.test.", func(z *v1alpha1.DNSZone) {
		z.Spec.Type = v1alpha1.ZoneTypeForward
		z.Spec.Forward = &v1alpha1.ForwardSpec{UpstreamGroup: "specific"}
	}))
	primary := build(t, dnsZone("leases.test."))
	for _, tc := range []struct {
		name              string
		zones             []*zone.Zone
		unsynced, decline bool
		qtype             uint16
		hook, next        int
		rcode             int
		group             string
	}{
		{name: "no configured zone", hook: 1},
		{name: "no zone hook falls through", decline: true, hook: 1, next: 1},
		{name: "root forward permits local", zones: []*zone.Zone{root(true, false)}, hook: 1},
		{name: "root forward fallback", zones: []*zone.Zone{root(true, false)}, decline: true, hook: 1, next: 1, group: "root"},
		{name: "specific forward wins", zones: []*zone.Zone{root(true, false), forward}, next: 1, group: "specific"},
		{name: "specific primary negative wins", zones: []*zone.Zone{root(true, false), primary}, rcode: dns.RcodeNameError},
		{name: "primary root negative wins", zones: []*zone.Zone{root(false, false)}, rcode: dns.RcodeNameError},
		{name: "no zone unsynced blocks local", unsynced: true, rcode: dns.RcodeServerFailure},
		{name: "root forward unsynced blocks local", zones: []*zone.Zone{root(true, false)}, unsynced: true, rcode: dns.RcodeServerFailure},
		{name: "root query ACL blocks local", zones: []*zone.Zone{root(true, true)}, rcode: dns.RcodeRefused},
		{name: "root forward transfer never invokes local", zones: []*zone.Zone{root(true, false)}, qtype: dns.TypeAXFR, rcode: dns.RcodeRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap, err := zone.NewSnapshot(1, !tc.unsynced, tc.zones...)
			if err != nil {
				t.Fatal(err)
			}
			holder := new(zone.Holder)
			holder.Store(snap)
			local := &testLocalRecords{handled: !tc.decline}
			stage := New(holder)
			stage.LocalRecords = local
			qtype := tc.qtype
			if qtype == 0 {
				qtype = dns.TypeA
			}
			msg := new(dns.Msg)
			msg.SetQuestion("host.leases.test.", qtype)
			req := &pipeline.Request{Msg: msg, Client: pipeline.Client{Addr: netip.MustParseAddrPort("192.0.2.5:12345"), Proto: pipeline.ProtoUDP}}
			next := new(recorder)
			response, err := stage.Serve(req, next.next)
			if err != nil {
				t.Fatal(err)
			}
			if local.calls != tc.hook || len(next.calls) != tc.next || response.Msg.Rcode != tc.rcode || req.UpstreamGroup != tc.group {
				t.Fatalf("hook=%d next=%d rcode=%d group=%q", local.calls, len(next.calls), response.Msg.Rcode, req.UpstreamGroup)
			}
			if tc.hook == 1 && !tc.decline && (!response.Msg.Authoritative || response.Status != pipeline.StatusLocalRecord) {
				t.Fatalf("local response altered: %+v", response)
			}
		})
	}
}
