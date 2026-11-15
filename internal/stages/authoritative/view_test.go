package authoritative

import (
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

func viewEP(name, typ, view string, targets ...string) v1alpha1.Endpoint {
	e := ep(name, typ, targets...)
	e.Kudurru = &v1alpha1.EndpointExt{View: view}
	return e
}

// viewSource serves split.example. with lan and kids views, an overlay zone
// with a lan-only name, and alias.example. whose CNAMEs lead into the split
// zone from outside it.
func viewSource(t testing.TB) *zone.Holder {
	t.Helper()
	split := build(t, dnsZone("split.example."),
		ep("www.split.example", "A", "203.0.113.10"),
		viewEP("www.split.example", "A", "lan", "10.0.0.10"),
		viewEP("www.split.example", "A", "kids", "10.9.0.10"),
		viewEP("nas.split.example", "A", "lan", "10.0.0.20"),
		viewEP("tunnel.split.example", "CNAME", "public", "tunnel.cloudflare.example"),
	)
	overlay := build(t, dnsZone("overlay.example.", func(dz *v1alpha1.DNSZone) {
		dz.Spec.Fallback = &v1alpha1.ZoneFallbackSpec{}
	}), ep("known.overlay.example", "A", "203.0.113.30"),
		viewEP("printer.overlay.example", "A", "lan", "10.0.0.40"))
	alias := build(t, dnsZone("alias.example."),
		ep("www.alias.example", "CNAME", "www.split.example"),
		ep("nas.alias.example", "CNAME", "nas.split.example"))
	snap, err := zone.NewSnapshot(1, true, split, overlay, alias)
	if err != nil {
		t.Fatal(err)
	}
	h := new(zone.Holder)
	h.Store(snap)
	return h
}

func viewRequest(qname string, qtype uint16, proto pipeline.Proto, groups ...string) *pipeline.Request {
	tc := Case{Qname: qname, Qtype: qtype, Proto: proto}
	req := tc.request()
	req.Client.GroupIDs = groups
	return req
}

func TestClientGroupViewsPerClientAnswers(t *testing.T) {
	st := New(viewSource(t))
	for _, proto := range []pipeline.Proto{pipeline.ProtoUDP, pipeline.ProtoTCP} {
		for _, tc := range []struct {
			name   string
			qname  string
			groups []string
			rcode  int
			answer []string
			next   bool
		}{
			{"default", "www.split.example.", nil, dns.RcodeSuccess, []string{"www.split.example.\t300\tIN\tA\t203.0.113.10"}, false},
			{"lan", "www.split.example.", []string{"dns/lan"}, dns.RcodeSuccess, []string{"www.split.example.\t300\tIN\tA\t10.0.0.10"}, false},
			{"kids", "www.split.example.", []string{"dns/kids"}, dns.RcodeSuccess, []string{"www.split.example.\t300\tIN\tA\t10.9.0.10"}, false},
			{"other group", "www.split.example.", []string{"dns/guests"}, dns.RcodeSuccess, []string{"www.split.example.\t300\tIN\tA\t203.0.113.10"}, false},
			{"view-only name for others", "nas.split.example.", []string{"dns/kids"}, dns.RcodeNameError, nil, false},
			{"view-only name for lan", "nas.split.example.", []string{"dns/lan"}, dns.RcodeSuccess, []string{"nas.split.example.\t300\tIN\tA\t10.0.0.20"}, false},
			{"public never served", "tunnel.split.example.", []string{"dns/lan"}, dns.RcodeNameError, nil, false},
			// A chain entering the split zone from another zone uses the
			// client's view there too.
			{"alias default", "www.alias.example.", nil, dns.RcodeSuccess, []string{"www.alias.example.\t300\tIN\tCNAME\twww.split.example.", "www.split.example.\t300\tIN\tA\t203.0.113.10"}, false},
			{"alias lan", "www.alias.example.", []string{"dns/lan"}, dns.RcodeSuccess, []string{"www.alias.example.\t300\tIN\tCNAME\twww.split.example.", "www.split.example.\t300\tIN\tA\t10.0.0.10"}, false},
			{"alias to view-only name", "nas.alias.example.", nil, dns.RcodeNameError, []string{"nas.alias.example.\t300\tIN\tCNAME\tnas.split.example."}, false},
			// A lan-only name in an overlay zone is answered locally for lan
			// and goes to the resolver for everyone else.
			{"overlay lan", "printer.overlay.example.", []string{"dns/lan"}, dns.RcodeSuccess, []string{"printer.overlay.example.\t300\tIN\tA\t10.0.0.40"}, false},
			{"overlay default", "printer.overlay.example.", nil, dns.RcodeSuccess, []string{"printer.overlay.example.\t60\tIN\tA\t" + upstreamA}, true},
		} {
			rec := &recorder{}
			req := viewRequest(tc.qname, dns.TypeA, proto, tc.groups...)
			resp, err := st.Serve(req, rec.next)
			if err != nil {
				t.Fatal(err)
			}
			got := sorted(resp.Msg.Answer)
			want := slices.Clone(tc.answer)
			slices.Sort(want)
			if resp.Msg.Rcode != tc.rcode || !slices.Equal(got, want) {
				t.Errorf("%v %s: rcode %d answer %v, want %d %v", proto, tc.name, resp.Msg.Rcode, got, tc.rcode, want)
			}
			if (len(rec.calls) > 0) != tc.next {
				t.Errorf("%v %s: rest of pipeline called %d times", proto, tc.name, len(rec.calls))
			}
			if tc.rcode == dns.RcodeNameError && (len(resp.Msg.Ns) != 1 || resp.Msg.Ns[0].Header().Rrtype != dns.TypeSOA) {
				t.Errorf("%v %s: negative answer without the default SOA: %v", proto, tc.name, resp.Msg.Ns)
			}
			// Group answers are final: never passed on, never cacheable.
			if !tc.next && resp.Cacheable {
				t.Errorf("%v %s: authoritative answer marked cacheable", proto, tc.name)
			}
		}
	}
}

// AXFR serves the default data whatever groups the requesting client is in.
func TestClientGroupViewsTransferDefaultData(t *testing.T) {
	st := New(viewSource(t))
	ch, err := st.Transfer("split.example.", 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for chunk := range ch {
		for _, rr := range chunk {
			names = append(names, rr.String())
		}
	}
	for _, rr := range names {
		for _, private := range []string{"10.0.0.", "10.9.0.", "nas.split", "tunnel"} {
			if strings.Contains(rr, private) {
				t.Fatalf("transfer leaked view data: %s", rr)
			}
		}
	}
	if !slices.Contains(names, "www.split.example.\t300\tIN\tA\t203.0.113.10") {
		t.Fatalf("transfer lost default data: %v", names)
	}
}
