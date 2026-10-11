package authoritative

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// gated returns a source over the test zones with gate g.
func gated(t testing.TB, g zone.Gate) *zone.Holder {
	t.Helper()
	h := source(t, true)
	h.Store(h.Load().WithGate(g))
	return h
}

// TestGateMatrix is the D80 matrix: what each kind of query gets while the
// zone cache is open, unsynced and serving a stale local.db.
func TestGateMatrix(t *testing.T) {
	type outcome struct {
		rcode    int
		status   pipeline.Status
		upstream string
		next     bool
	}
	servfail := func(up string) outcome {
		return outcome{dns.RcodeServerFailure, pipeline.StatusServfailUnsynced, up, false}
	}
	auth := outcome{dns.RcodeSuccess, pipeline.StatusAuthoritative, "zone:example.org.", false}
	fwd := outcome{dns.RcodeSuccess, pipeline.StatusForwarded, upstreamID, true}
	rows := []struct {
		name         string
		qname        string
		qtype        uint16
		open         outcome
		unsynced     outcome
		stale        outcome
		staleUpGroup string
	}{
		{"primary-positive", "web.example.org.", dns.TypeA, auth, auth, servfail(UpstreamStale), ""},
		{"primary-nxdomain", "nope.example.org.", dns.TypeA,
			outcome{dns.RcodeNameError, pipeline.StatusAuthoritative, "zone:example.org.", false}, servfail(UpstreamUnsynced), servfail(UpstreamStale), ""},
		{"primary-nodata", "web.example.org.", dns.TypeAAAA, auth, servfail(UpstreamUnsynced), servfail(UpstreamStale), ""},
		{"primary-cname-leaves", "ext.example.org.", dns.TypeA,
			outcome{dns.RcodeSuccess, pipeline.StatusForwarded, upstreamID, true}, servfail(UpstreamUnsynced), servfail(UpstreamStale), ""},
		{"forward-zone", "app.corp.example.", dns.TypeA, fwd, servfail(UpstreamUnsynced), fwd, "corp"},
		{"no-zone", "www.external.test.", dns.TypeA, fwd, servfail(UpstreamUnsynced), fwd, ""},
	}
	for _, row := range rows {
		for _, g := range []struct {
			gate zone.Gate
			want outcome
		}{{zone.GateOpen, row.open}, {zone.GateUnsynced, row.unsynced}, {zone.GateStale, row.stale}} {
			t.Run(row.name+"/"+g.gate.String(), func(t *testing.T) {
				rec := &recorder{}
				tc := Case{Qname: row.qname, Qtype: row.qtype}
				resp, err := New(gated(t, g.gate)).Serve(tc.request(), rec.next)
				if err != nil {
					t.Fatal(err)
				}
				w := g.want
				if resp.Msg.Rcode != w.rcode || resp.Status != w.status || resp.Upstream != w.upstream {
					t.Fatalf("got %s/%s/%q, want %s/%s/%q", dns.RcodeToString[resp.Msg.Rcode], resp.Status, resp.Upstream,
						dns.RcodeToString[w.rcode], w.status, w.upstream)
				}
				if (len(rec.calls) > 0) != w.next {
					t.Fatalf("next called %d times, want called=%v", len(rec.calls), w.next)
				}
				if g.gate == zone.GateStale && w.next && rec.calls[0].UpstreamGroup != row.staleUpGroup {
					t.Fatalf("group hint %q, want %q", rec.calls[0].UpstreamGroup, row.staleUpGroup)
				}
			})
		}
	}
	if New(gated(t, zone.GateStale)).Ready() {
		t.Fatal("a stale set is not Ready")
	}
}

// xfrSource holds one primary zone with the given transfer policy and a
// forward zone.
func xfrSource(t testing.TB, p v1alpha1.ZonePolicy, records int) (*zone.Holder, *zone.Zone) {
	t.Helper()
	eps := []v1alpha1.Endpoint{ep("ns1.example.org", "A", "192.0.2.1")}
	for i := range records {
		eps = append(eps, ep(fmt.Sprintf("h%04d.example.org", i), "TXT", "some text to make the zone larger than one small message"))
	}
	z := build(t, dnsZone("example.org.", func(dz *v1alpha1.DNSZone) { dz.Spec.Policy = p }), eps...)
	corp := build(t, dnsZone("corp.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Type = v1alpha1.ZoneTypeForward
		dz.Spec.Policy.ZoneTransfer = v1alpha1.AccessAllow
	}))
	snap, err := zone.NewSnapshot(1, true, z, corp)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	return h, z
}

func xfrRequest(qname string, qtype uint16, proto pipeline.Proto, client, key string, edns bool) *pipeline.Request {
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)
	if edns {
		m.SetEdns0(4096, false)
	}
	return &pipeline.Request{Msg: m, Client: pipeline.Client{Addr: netip.AddrPortFrom(netip.MustParseAddr(client), 5353), Proto: proto, TSIGKey: key}}
}

func ixfrRequest(qname string, serial uint32, proto pipeline.Proto, client, key string) *pipeline.Request {
	req := xfrRequest(qname, dns.TypeIXFR, proto, client, key, false)
	req.Msg.SetIxfr(qname, serial, "ns1."+qname, "hostmaster."+qname)
	return req
}

// stream collects every message of a response the way a stream transport
// writes it.
func stream(resp *pipeline.Response) []*dns.Msg {
	if resp.Transfer == nil {
		return []*dns.Msg{resp.Msg}
	}
	return slices.Collect(resp.Transfer)
}

func answers(msgs []*dns.Msg) []string {
	var out []string
	for _, m := range msgs {
		for _, r := range m.Answer {
			out = append(out, r.String())
		}
	}
	return out
}

func zoneStream(z *zone.Zone) []string {
	var out []string
	for c := range z.AXFR(0) {
		for _, r := range c {
			out = append(out, r.String())
		}
	}
	return out
}

func TestTransferPolicy(t *testing.T) {
	h, z := xfrSource(t, v1alpha1.ZonePolicy{
		ZoneTransfer: v1alpha1.AccessUseNetworkACL, TransferACL: []string{"10.0.0.0/8"},
		TransferTSIGKeyRefs: []string{"xfr-key."},
	}, 50)
	st := New(h)
	st.Metrics = NewMetrics(nil)
	const lan, wan, key = "10.0.0.2", "198.51.100.1", "xfr-key."
	full := zoneStream(z)
	soa := []string{z.SOA().String()}
	for _, tc := range []struct {
		name   string
		req    *pipeline.Request
		rcode  int
		status pipeline.Status
		want   []string // the answer stream, nil for none
		result string
	}{
		{"axfr-tcp-key", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, lan, key, false), dns.RcodeSuccess, pipeline.StatusAuthoritative, full, TransferSuccess},
		{"axfr-dot-key", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoDoT, lan, key, true), dns.RcodeSuccess, pipeline.StatusAuthoritative, full, TransferSuccess},
		{"axfr-no-key", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, lan, "", false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, TransferRefused},
		{"axfr-wrong-key", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, lan, "other.", false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, TransferRefused},
		{"axfr-key-wrong-address", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, wan, key, false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, TransferRefused},
		{"axfr-udp", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoUDP, lan, key, false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, TransferRefused},
		{"axfr-doh", xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoDoH, lan, key, false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, TransferRefused},
		{"ixfr-current", ixfrRequest("example.org.", z.Serial(), pipeline.ProtoTCP, lan, key), dns.RcodeSuccess, pipeline.StatusAuthoritative, soa, TransferUpToDate},
		{"ixfr-newer", ixfrRequest("example.org.", z.Serial()+5, pipeline.ProtoUDP, lan, key), dns.RcodeSuccess, pipeline.StatusAuthoritative, soa, TransferUpToDate},
		{"ixfr-old-tcp", ixfrRequest("example.org.", z.Serial()-1, pipeline.ProtoTCP, lan, key), dns.RcodeSuccess, pipeline.StatusAuthoritative, full, TransferSuccess},
		{"ixfr-old-udp", ixfrRequest("example.org.", z.Serial()-1, pipeline.ProtoUDP, lan, key), dns.RcodeSuccess, pipeline.StatusAuthoritative, soa, TransferTCPRequired},
		{"ixfr-no-soa", xfrRequest("example.org.", dns.TypeIXFR, pipeline.ProtoTCP, lan, key, false), dns.RcodeFormatError, pipeline.StatusAuthoritative, nil, TransferFormErr},
		{"forward-zone", xfrRequest("corp.example.", dns.TypeAXFR, pipeline.ProtoTCP, lan, key, false), dns.RcodeRefused, pipeline.StatusRefusedACL, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := st.Serve(tc.req, (&recorder{}).next)
			if err != nil {
				t.Fatal(err)
			}
			if resp.Msg.Rcode != tc.rcode || resp.Status != tc.status {
				t.Fatalf("got %s/%s, want %s/%s", dns.RcodeToString[resp.Msg.Rcode], resp.Status, dns.RcodeToString[tc.rcode], tc.status)
			}
			msgs := stream(resp)
			if got := answers(msgs); !slices.Equal(got, tc.want) {
				t.Fatalf("answer stream of %d RRs, want %d", len(got), len(tc.want))
			}
			for i, m := range msgs {
				if m.Id != tc.req.Msg.Id || len(m.Question) != 1 || (tc.rcode == dns.RcodeSuccess && !m.Authoritative) {
					t.Fatalf("message %d header %v", i, m.MsgHdr)
				}
				if (m.IsEdns0() != nil) != (tc.req.Msg.IsEdns0() != nil) {
					t.Fatalf("message %d OPT echo", i)
				}
			}
			if resp.Transfer != nil && msgs[0] != resp.Msg {
				t.Fatal("Msg must be the first message of the stream")
			}
			if tc.result != "" {
				if n := testutil.ToFloat64(st.Metrics.Transfers.WithLabelValues("example.org.", "out", tc.result)); n < 1 {
					t.Fatalf("result %s not counted", tc.result)
				}
			}
		})
	}

	// The query ACL is checked before the transfer policy.
	hq, _ := xfrSource(t, v1alpha1.ZonePolicy{QueryAccess: v1alpha1.AccessDeny, ZoneTransfer: v1alpha1.AccessAllow}, 1)
	resp, _ := New(hq).Serve(xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, lan, "", false), (&recorder{}).next)
	if resp.Msg.Rcode != dns.RcodeRefused || resp.Transfer != nil {
		t.Fatal("queryAccess deny must refuse transfers too")
	}
	// Nothing is transferred while the gate is not open.
	for _, g := range []zone.Gate{zone.GateUnsynced, zone.GateStale} {
		hg, _ := xfrSource(t, v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}, 1)
		hg.Store(hg.Load().WithGate(g))
		resp, _ := New(hg).Serve(xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, lan, "", false), (&recorder{}).next)
		if resp.Msg.Rcode != dns.RcodeServerFailure || resp.Status != pipeline.StatusServfailUnsynced || resp.Transfer != nil {
			t.Fatalf("%s: %v %s", g, resp.Msg.MsgHdr, resp.Status)
		}
	}
}

func TestTransferStreamChunksAndAbort(t *testing.T) {
	h, z := xfrSource(t, v1alpha1.ZonePolicy{ZoneTransfer: v1alpha1.AccessAllow}, 400)
	st := New(h)
	st.Metrics = NewMetrics(nil)
	st.ChunkBytes = 2048
	resp, err := st.Serve(xfrRequest("example.org.", dns.TypeAXFR, pipeline.ProtoTCP, "192.0.2.9", "", false), (&recorder{}).next)
	if err != nil {
		t.Fatal(err)
	}
	msgs := stream(resp)
	if len(msgs) < 10 {
		t.Fatalf("%d messages with 2 KiB chunks", len(msgs))
	}
	for i, m := range msgs {
		packed, err := m.Pack()
		if err != nil || len(packed) > 2048+zone.TransferOverhead {
			t.Fatalf("message %d: %d octets, %v", i, len(packed), err)
		}
	}
	if !slices.Equal(answers(msgs), zoneStream(z)) {
		t.Fatal("stream differs from the zone")
	}
	// A client that goes away mid-stream is counted as aborted.
	for range resp.Transfer {
		break
	}
	if n := testutil.ToFloat64(st.Metrics.Transfers.WithLabelValues("example.org.", "out", TransferAborted)); n != 1 {
		t.Fatalf("aborted %v", n)
	}
	if n := testutil.ToFloat64(st.Metrics.Transfers.WithLabelValues("example.org.", "out", TransferSuccess)); n != 1 {
		t.Fatalf("success %v", n)
	}
}

func TestTransferCapability(t *testing.T) {
	h, z := xfrSource(t, v1alpha1.ZonePolicy{}, 100)
	var tp TransferProvider = New(h)
	ch, err := tp.Transfer("EXAMPLE.org", 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for c := range ch {
		for _, r := range c {
			got = append(got, r.String())
		}
	}
	if !slices.Equal(got, zoneStream(z)) {
		t.Fatal("capability stream differs from the zone")
	}
	ch, err = tp.Transfer("example.org.", z.Serial())
	if err != nil {
		t.Fatal(err)
	}
	if c := <-ch; len(c) != 1 || c[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("current serial: %v", c)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel must be closed after the SOA")
	}
	for name, want := range map[string]error{
		"www.example.org.": ErrZoneNotFound,
		"example.net.":     ErrZoneNotFound,
		"corp.example.":    ErrNotTransferable,
	} {
		if _, err := tp.Transfer(name, 0); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", name, err, want)
		}
	}
	h.Store(h.Load().WithGate(zone.GateStale))
	if _, err := tp.Transfer("example.org.", 0); !errors.Is(err, ErrUnsynced) {
		t.Fatalf("stale: %v", err)
	}
}
