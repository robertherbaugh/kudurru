package log

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// TestObserverStampsKeptTraces: a kept trace puts its id on the query
// event and an exemplar on the latency histogram; a trace that was not
// kept leaves no id (it links nowhere), and the TRACEPARENT flag records
// that the query carried the option either way.
func TestObserverStampsKeptTraces(t *testing.T) {
	tracer, err := tracing.New(tracing.Config{Ratio: 0, Exporter: &tracing.Memory{}})
	if err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	rec := &recorder{}
	o := &Observer{Node: "node-a", Collector: rec, Metrics: metrics.NewDNS(reg)}

	for _, keep := range []bool{true, false} {
		req := request("www.example.com.", dns.TypeA)
		req.Trace, _ = tracer.Begin(req.Msg, false, req.Received)
		req.Traceparent = true
		req.Trace.Decide(time.Millisecond, keep)
		req.Trace.Finish(req.Received.Add(time.Millisecond))
		o.Observe(req, &pipeline.Response{Msg: reply(req, dns.RcodeSuccess), Status: pipeline.StatusForwarded}, nil, time.Millisecond)
		ev := rec.events[len(rec.events)-1]
		if ev.Traced() != keep || (keep && ev.TraceID != req.Trace.ID()) || !ev.Flags.Traceparent {
			t.Fatalf("keep %v: trace id %x flag %v", keep, ev.TraceID, ev.Flags.Traceparent)
		}
		if keep {
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range families {
				for _, m := range f.GetMetric() {
					for _, b := range m.GetHistogram().GetBucket() {
						for _, l := range b.GetExemplar().GetLabel() {
							found = found || (l.GetName() == "trace_id" && l.GetValue() == req.Trace.ID().String())
						}
					}
				}
			}
			if !found {
				t.Fatal("no exemplar for the kept trace")
			}
		}
	}
}
