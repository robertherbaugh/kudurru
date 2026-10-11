// Package log is the final pipeline step, implemented as a pipeline.Observer
// because it must see the final response: it builds the query event,
// enqueues it without blocking, and updates the Prometheus instruments
// (docs/architecture.md §7.1 step 13).
package log

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/metrics"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
)

// RcodeNone is the rcode label of kudurru_dns_responses_total for queries
// that were dropped without a response (rate-limit drops, control-stage
// drops; the transports send nothing).
const RcodeNone = "none"

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
	// What the transport sends: nothing for a drop (dnsserver.IsDrop and
	// pipeline.ErrDrop), so the rcode label is "none", not SERVFAIL.
	rcodeLabel := metrics.RcodeLabel(rcode)
	if errors.Is(err, pipeline.ErrDrop) || (err == nil && resp != nil && resp.Msg == nil &&
		(resp.Status == pipeline.StatusDropped || resp.Status == pipeline.StatusBlockedRateLimit)) {
		rcodeLabel = RcodeNone
	}

	if o.Metrics != nil {
		o.Metrics.Requests.WithLabelValues(proto, metrics.QTypeLabel(q.Qtype)).Inc()
		o.Metrics.Responses.WithLabelValues(proto, rcodeLabel, status.String()).Inc()
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
	ev.ClientName = req.Client.ClientID
	if len(req.Client.GroupIDs) > 0 {
		ev.ClientGroup = req.Client.GroupIDs[0]
	}
	ev.Zone = zoneName(req.Zone)
	if req.Msg != nil {
		ev.Flags.RD = req.Msg.RecursionDesired
		ev.Flags.CD = req.Msg.CheckingDisabled
		if opt := req.Msg.IsEdns0(); opt != nil {
			ev.Flags.DO = opt.Do()
			for _, o := range opt.Option {
				if sn, ok := o.(*dns.EDNS0_SUBNET); ok {
					if a, ok := netip.AddrFromSlice(sn.Address); ok {
						if p, err := a.Unmap().Prefix(int(sn.SourceNetmask)); err == nil {
							ev.ECS = p
							ev.Flags.ECS = true
						}
					}
				}
			}
		}
	}
	ev.ReplyType = querylog.ReplyNone
	if resp != nil && resp.Msg != nil {
		ev.Flags.TC = resp.Msg.Truncated
		ev.Flags.AD = resp.Msg.AuthenticatedData
		ev.ReplyType = replyType(q.Qtype, resp.Msg)
		ev.Answer = answerSummary(resp.Msg.Answer)
		if opt := resp.Msg.IsEdns0(); opt != nil {
			for _, o := range opt.Option {
				if e, ok := o.(*dns.EDNS0_EDE); ok {
					ev.EDE, ev.HasEDE = e.InfoCode, true
					break
				}
			}
		}
	}
	ev.Flags.Cached = status == pipeline.StatusCached || status == pipeline.StatusCachedStale
	ev.Flags.Stale = status == pipeline.StatusCachedStale
	ev.Flags.Prefetch = status == pipeline.StatusPrefetchRefresh
	o.Collector.Enqueue(ev)
}

// zoneName returns the name of the zone the authoritative stage matched
// (Request.Zone is an opaque handle; a *zone.Zone has Name()).
func zoneName(z any) (name string) {
	n, ok := z.(interface{ Name() string })
	if !ok {
		return ""
	}
	defer func() {
		if recover() != nil { // typed nil handle
			name = ""
		}
	}()
	return strings.TrimSuffix(n.Name(), ".")
}

// replyType classifies the response for the query log.
func replyType(qtype uint16, m *dns.Msg) querylog.ReplyType {
	switch m.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		return querylog.ReplyNXDomain
	case dns.RcodeServerFailure:
		return querylog.ReplyServfail
	case dns.RcodeRefused:
		return querylog.ReplyRefused
	case dns.RcodeNotImplemented:
		return querylog.ReplyNotImp
	default:
		return querylog.ReplyOther
	}
	if len(m.Answer) == 0 {
		return querylog.ReplyNoData
	}
	sawCNAME := false
	for _, rr := range m.Answer {
		switch rr.(type) {
		case *dns.A, *dns.AAAA:
			return querylog.ReplyIP
		case *dns.CNAME, *dns.DNAME:
			sawCNAME = true
		}
	}
	switch {
	case qtype == dns.TypePTR || qtype == dns.TypeNS:
		return querylog.ReplyDomain
	case sawCNAME && (qtype == dns.TypeA || qtype == dns.TypeAAAA || qtype == dns.TypeCNAME):
		return querylog.ReplyCNAME
	}
	return querylog.ReplyBlob
}

// answerSummaryRRs and answerSummaryBytes bound the stored answer summary.
const (
	answerSummaryRRs   = 3
	answerSummaryBytes = 256
)

// answerSummary renders the first answer records compactly: addresses as
// is, other types as "TYPE rdata". The writer stores it only under privacy
// full (D59).
func answerSummary(answer []dns.RR) string {
	if len(answer) == 0 {
		return ""
	}
	var b strings.Builder
	for i, rr := range answer {
		if i == answerSummaryRRs {
			fmt.Fprintf(&b, " (+%d)", len(answer)-i)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		switch v := rr.(type) {
		case *dns.A:
			b.WriteString(v.A.String())
		case *dns.AAAA:
			b.WriteString(v.AAAA.String())
		default:
			h := rr.Header()
			b.WriteString(dns.TypeToString[h.Rrtype])
			b.WriteByte(' ')
			b.WriteString(strings.TrimPrefix(rr.String(), h.String()))
		}
		if b.Len() >= answerSummaryBytes {
			break
		}
	}
	s := b.String()
	if len(s) > answerSummaryBytes {
		s = s[:answerSummaryBytes]
	}
	return s
}
