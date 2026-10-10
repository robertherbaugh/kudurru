// Package log is the final pipeline step, implemented as a pipeline.Observer
// because it must see the final response: it builds the query event,
// enqueues it without blocking, and updates the Prometheus instruments
// (docs/architecture.md §7.1 step 13).
package log

import (
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

// Observer implements pipeline.Observer.
type Observer struct {
	Node      string
	Collector querylog.Collector
	Metrics   *metrics.DNS
}

// Observe implements pipeline.Observer.
func (o *Observer) Observe(req *pipeline.Request, resp *pipeline.Response, err error, latency time.Duration) {
	q := req.Question()
	proto := req.Client.Proto.String()

	status := pipeline.StatusUnknown
	rcode := dns.RcodeServerFailure
	answers := 0
	var upstream, list, rule string
	if resp != nil {
		status = resp.Status
		upstream, list, rule = resp.Upstream, resp.List, resp.Rule
		if resp.Msg != nil {
			rcode = resp.Msg.Rcode
			answers = len(resp.Msg.Answer)
		}
	} else if err != nil {
		status = pipeline.StatusDropped
	}

	if o.Metrics != nil {
		o.Metrics.Requests.WithLabelValues(proto, metrics.QTypeLabel(q.Qtype)).Inc()
		o.Metrics.Responses.WithLabelValues(proto, metrics.RcodeLabel(rcode), status.String()).Inc()
		o.Metrics.Duration.WithLabelValues(proto, string(status.Class())).Observe(latency.Seconds())
	}

	// The health name is probed every second by dnsdist and keepalived and
	// is never logged.
	if o.Collector == nil || strings.EqualFold(q.Name, ingress.HealthName) {
		return
	}
	ev := &querylog.Event{
		Time:     req.Received,
		Latency:  latency,
		Node:     o.Node,
		Proto:    req.Client.Proto,
		Client:   req.Client.Addr.Addr(),
		Name:     strings.ToLower(q.Name),
		QType:    q.Qtype,
		QClass:   q.Qclass,
		Status:   status,
		Rcode:    rcode,
		Upstream: upstream,
		List:     list,
		Rule:     rule,
		Answers:  answers,
	}
	if req.Msg != nil {
		ev.Flags.RD = req.Msg.RecursionDesired
		ev.Flags.CD = req.Msg.CheckingDisabled
		if opt := req.Msg.IsEdns0(); opt != nil {
			ev.Flags.DO = opt.Do()
		}
	}
	if resp != nil && resp.Msg != nil {
		ev.Flags.TC = resp.Msg.Truncated
		ev.Flags.AD = resp.Msg.AuthenticatedData
	}
	ev.Flags.Cached = status == pipeline.StatusCached || status == pipeline.StatusCachedStale
	ev.Flags.Stale = status == pipeline.StatusCachedStale
	o.Collector.Enqueue(ev)
}
