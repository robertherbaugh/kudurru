// Package metrics holds the Prometheus instruments. Labels follow the
// bounded-cardinality policy of dev-docs/design-record.md §11.1: client and
// domain are never labels.
package metrics

import (
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// DNS instruments the query path.
type DNS struct {
	Requests  *prometheus.CounterVec
	Responses *prometheus.CounterVec
	Duration  *prometheus.HistogramVec
	Inflight  *prometheus.GaugeVec
	Dropped   *prometheus.CounterVec
}

// NewDNS registers the DNS metrics with reg.
func NewDNS(reg prometheus.Registerer) *DNS {
	d := &DNS{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_dns_requests_total",
			Help: "DNS requests received, by transport and query type.",
		}, []string{"proto", "qtype"}),
		Responses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_dns_responses_total",
			Help: "DNS responses sent, by transport, rcode and pipeline status.",
		}, []string{"proto", "rcode", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_dns_request_duration_seconds",
			Help:    "Time from receipt to response.",
			Buckets: []float64{.0005, .001, .002, .005, .01, .025, .05, .1, .25, .5, 1},
		}, []string{"proto", "status_class"}),
		Inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_dns_inflight_requests",
			Help: "Requests currently being processed.",
		}, []string{"proto"}),
		Dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_dns_dropped_total",
			Help: "Queries dropped before the pipeline, by reason (proxy_unauthenticated, proxy_untrusted_peer, proxy_malformed, ...).",
		}, []string{"reason"}),
	}
	reg.MustRegister(d.Requests, d.Responses, d.Duration, d.Inflight, d.Dropped)
	return d
}

// QTypeLabel maps a qtype onto the fixed label set.
func QTypeLabel(t uint16) string {
	switch t {
	case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeNS, dns.TypePTR,
		dns.TypeSOA, dns.TypeSRV, dns.TypeTXT, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeCAA,
		dns.TypeDS, dns.TypeDNSKEY, dns.TypeNAPTR, dns.TypeTLSA:
		return dns.TypeToString[t]
	}
	return "other"
}

// RcodeLabel maps an rcode onto its name or "other".
func RcodeLabel(rc int) string {
	if s, ok := dns.RcodeToString[rc]; ok {
		return s
	}
	return "other"
}
