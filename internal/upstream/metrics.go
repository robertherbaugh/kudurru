package upstream

import (
	"net/url"
	"strings"

	proxy "github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the upstream instruments. The upstream label is
// "<group>/<server name>", the server name being the display name or the
// address as configured, so cardinality is bounded by the configuration and
// resolved IPs never appear (docs/architecture.md §11.1).
type Metrics struct {
	Healthy *prometheus.GaugeVec
	// Requests is kudurru_upstream_requests_total{upstream,proto}; proto
	// is the transport of the server (udp, tcp, dot, doh, doh3, doq,
	// dnscrypt).
	Requests *prometheus.CounterVec
	// Responses is kudurru_upstream_responses_total{upstream,rcode}: every
	// response received, whatever its rcode.
	Responses *prometheus.CounterVec
	// Inflight is kudurru_upstream_inflight{upstream}: exchanges waiting
	// for this server now.
	Inflight *prometheus.GaugeVec
	Errors   *prometheus.CounterVec
	Duration *prometheus.HistogramVec
}

// NewMetrics registers the upstream metrics with reg. Create them once per
// process and pass them to every Set (reloads build new Sets).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Healthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_upstream_healthy",
			Help: "1 when the upstream server is healthy, 0 while it is in failure backoff.",
		}, []string{"upstream"}),
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_upstream_requests_total",
			Help: "Exchanges attempted with an upstream server, by server and transport.",
		}, []string{"upstream", "proto"}),
		Responses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_upstream_responses_total",
			Help: "Responses received from an upstream server, by rcode.",
		}, []string{"upstream", "rcode"}),
		Inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kudurru_upstream_inflight",
			Help: "Exchanges currently waiting for an upstream server.",
		}, []string{"upstream"}),
		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kudurru_upstream_errors_total",
			Help: "Failed exchanges with an upstream server, by error class (timeout, network, refused, servfail).",
		}, []string{"upstream", "error"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "kudurru_upstream_request_duration_seconds",
			Help:    "Round-trip time of exchanges with an upstream server that returned a response.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5},
		}, []string{"upstream"}),
	}
	if reg != nil {
		reg.MustRegister(m.Healthy, m.Requests, m.Responses, m.Inflight, m.Errors, m.Duration)
	}
	return m
}

// forget removes the series of a server that is no longer configured.
func (m *Metrics) forget(label string) {
	if m == nil {
		return
	}
	m.Healthy.DeleteLabelValues(label)
	m.Inflight.DeleteLabelValues(label)
	m.Requests.DeletePartialMatch(prometheus.Labels{"upstream": label})
	m.Responses.DeletePartialMatch(prometheus.Labels{"upstream": label})
	m.Duration.DeleteLabelValues(label)
	for _, r := range []string{reasonTimeout, reasonNetwork, reasonRefused, reasonServfail} {
		m.Errors.DeleteLabelValues(label, r)
	}
}

// Error reasons.
const (
	reasonTimeout  = "timeout"
	reasonNetwork  = "network"
	reasonRefused  = "refused"
	reasonServfail = "servfail"
)

// protoLabel is the transport of a server, from the scheme of its
// configured address: a bare host:port is plain DNS (UDP, with TCP
// fallback on truncation), the others name their transport. An sdns://
// stamp is labelled by the upstream dnsproxy built from it (whose Address
// keeps sdns:// for DNSCrypt and is an https:// URL for a DoH stamp).
func protoLabel(configured string, u proxy.Upstream) string {
	scheme := schemeOf(configured)
	if scheme == "sdns" && u != nil {
		if s := schemeOf(u.Address()); s != "" {
			scheme = s
		}
	}
	switch scheme {
	case "":
		return "udp"
	case "udp":
		return "udp"
	case "tcp":
		return "tcp"
	case "tls":
		return "dot"
	case "https":
		return "doh"
	case "h3":
		return "doh3"
	case "quic":
		return "doq"
	case "sdns":
		return "dnscrypt"
	}
	return "other"
}

// schemeOf returns the lower-cased URL scheme of addr, or "" for a bare
// host[:port].
func schemeOf(addr string) string {
	i := strings.Index(addr, "://")
	if i <= 0 {
		return ""
	}
	if pu, err := url.Parse(addr); err == nil && pu.Scheme != "" {
		return strings.ToLower(pu.Scheme)
	}
	return strings.ToLower(addr[:i])
}

// rcodeLabel maps an rcode onto its name or "other".
func rcodeLabel(rc int) string {
	if s, ok := dns.RcodeToString[rc]; ok {
		return s
	}
	return "other"
}
