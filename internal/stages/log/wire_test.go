package log

import (
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// wireRecorder is a collector that reports whether it retains wire
// messages, as a route with a raw sink does.
type wireRecorder struct {
	recorder
	retain bool
	asked  int
}

func (r *wireRecorder) RetainsWire() bool { r.asked++; return r.retain }

// TestObserverRetainsWireOnlyForARawRoute: the query and the response are
// packed only for a collector that asks for them (D372), so the default
// path packs nothing; the packed messages are the ones of the exchange.
func TestObserverRetainsWireOnlyForARawRoute(t *testing.T) {
	for _, retain := range []bool{false, true} {
		rec := &wireRecorder{retain: retain}
		o := &Observer{Node: "node-a", Collector: rec}
		req := request("www.example.com.", dns.TypeA)
		req.Msg.Id = 777
		resp := reply(req, dns.RcodeSuccess, "www.example.com. 60 IN A 192.0.2.1")
		o.Observe(req, &pipeline.Response{Msg: resp, Status: pipeline.StatusForwarded}, nil, time.Millisecond)
		if len(rec.events) != 1 || rec.asked != 1 {
			t.Fatalf("events %d, asked %d", len(rec.events), rec.asked)
		}
		w := rec.events[0].Wire
		if !retain {
			if w != nil {
				t.Fatal("a route without a raw sink retained wire messages")
			}
			continue
		}
		if w == nil || w.ClientPort != 5353 {
			t.Fatalf("wire %+v", w)
		}
		q, a := new(dns.Msg), new(dns.Msg)
		if err := q.Unpack(w.Query); err != nil || q.Id != 777 || q.Question[0].Name != "www.example.com." {
			t.Fatalf("query %v %v", q, err)
		}
		if err := a.Unpack(w.Response); err != nil || !a.Response || len(a.Answer) != 1 {
			t.Fatalf("response %v %v", a, err)
		}
	}
	// An extended rcode packs from a copy: the response is left unchanged.
	rec := &wireRecorder{retain: true}
	req := request("www.example.com.", dns.TypeA)
	resp := reply(req, dns.RcodeSuccess)
	resp.SetEdns0(1232, false)
	resp.Rcode = dns.RcodeBadVers
	(&Observer{Collector: rec}).Observe(req, &pipeline.Response{Msg: resp, Status: pipeline.StatusForwarded}, nil, time.Millisecond)
	if resp.IsEdns0().ExtendedRcode() != 0 {
		t.Fatal("packing the wire messages changed the response")
	}
	if a := new(dns.Msg); rec.events[0].Wire.Response == nil || a.Unpack(rec.events[0].Wire.Response) != nil || a.Rcode != dns.RcodeBadVers {
		t.Fatal("extended rcode response")
	}
	var _ querylog.WireRetainer = rec
}
