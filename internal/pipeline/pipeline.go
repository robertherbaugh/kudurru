// Package pipeline defines the fixed query path: a chain of named stages
// with middleware semantics (each stage may act before and after calling
// next) and observers that see the final response (docs/architecture.md
// §3.3, §7.1).
//
// Stage order is code, not configuration: New rejects stages that appear in
// an order other than Order. Stages may be omitted, never reordered.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/netip"
	"time"

	"github.com/miekg/dns"
)

// Order is the fixed stage order. "log" is implemented as an Observer
// because it must see the final response; it is listed for documentation.
var Order = []string{
	"ingress", "identify", "control", "local", "authoritative", "blocking",
	"rewrite", "cache", "dnssec", "upstream", "postprocess", "log",
}

// ErrNoAnswer is returned when every stage called next and nothing answered.
var ErrNoAnswer = errors.New("pipeline: no stage produced an answer")

// ErrDrop is returned (with a nil Response) by a stage that decided the
// query must not be answered at all: the control stage drops malformed
// queries this way. Observers record such queries as StatusDropped.
// Transports should send nothing for UDP and DoQ and close the TCP, DoT,
// DoH or DoH3 exchange; a transport that does not check for ErrDrop
// answers SERVFAIL as it does for any error.
var ErrDrop = errors.New("pipeline: query dropped")

// Proto is the transport the query arrived on.
type Proto uint8

// Transports.
const (
	ProtoUDP Proto = iota
	ProtoTCP
	ProtoDoT
	ProtoDoH
	ProtoDoQ
	ProtoDoH3
)

func (p Proto) String() string {
	switch p {
	case ProtoUDP:
		return "udp"
	case ProtoTCP:
		return "tcp"
	case ProtoDoT:
		return "dot"
	case ProtoDoH:
		return "doh"
	case ProtoDoQ:
		return "doq"
	case ProtoDoH3:
		return "doh3"
	}
	return "unknown"
}

// Client describes who asked. ProxiedFrom is the dnsdist address when the
// query arrived with an authenticated PROXYv2 header.
type Client struct {
	Addr        netip.AddrPort
	Proto       Proto
	ProxiedFrom netip.Addr
	ClientID    string
	MACs        []string
	// GroupIDs are the client's ClientGroups ("namespace/name", sorted),
	// set by the identify stage. The slice is shared with the policy
	// index: read it, never modify it.
	GroupIDs []string
	// TSIGKey is the lower-case, absolute name of the TSIG key that signed
	// the query, set only after the transport verified the signature (RFC
	// 8945). Queries whose signature fails verification never reach the
	// pipeline; the transport answers them NOTAUTH itself.
	TSIGKey string
}

// Request is the per-query context handed through the stages.
type Request struct {
	Ctx      context.Context
	Msg      *dns.Msg
	Client   Client
	Received time.Time
	Deadline time.Time
	// Zone is set by the authoritative stage when a zone matched. It is an
	// opaque handle here to keep miekg types out of the zone model; the
	// authoritative stage stores a *zone.Zone.
	Zone any
	// UpstreamGroup is the upstream group an earlier stage chose for this
	// query: the authoritative stage sets it for names in a forward zone
	// (DNSZone spec.forward.upstreamGroup, default "default"), and a
	// Policy's upstreamGroup may set it later. The upstream stage forwards
	// to this group when it is non-empty and to its default group
	// otherwise. It is a group name from the Kudurru settings, never an
	// address, and is recorded in Response.Upstream by the upstream stage.
	// It takes precedence over the conditional-forwarding map, and answers
	// forwarded on its behalf are never cached.
	UpstreamGroup string
	// Policy is the client's policy decision, set by the identify stage
	// for later stages (control, blocking, and v0.2 rewrite/safe-search).
	// It holds a *policy.Decision (internal/policy; read it with
	// policy.FromRequest). Like Zone it is an opaque handle here, because
	// internal/policy depends on this package. The value is shared and
	// immutable: stages must not modify it.
	Policy any
}

// Question returns the first question or a zero value.
func (r *Request) Question() dns.Question {
	if r.Msg != nil && len(r.Msg.Question) > 0 {
		return r.Msg.Question[0]
	}
	return dns.Question{}
}

// Response is what a stage produces.
type Response struct {
	Msg       *dns.Msg
	Status    Status
	Upstream  string // dictionary name, never a raw IP
	List      string // matched list for the UI "why" row
	Rule      string // matched rule text
	Cacheable bool
	// Transfer, when set, is a multi-message answer (an AXFR or IXFR
	// stream, RFC 5936 §2.2). Stream transports (TCP, DoT) write every
	// message it yields, in order, instead of Msg; Msg is the first of
	// them, kept for observers and for transports that cannot stream.
	// Stages set it only for requests that arrived over TCP or DoT.
	Transfer iter.Seq[*dns.Msg]
}

// Next calls the rest of the chain.
type Next func(*Request) (*Response, error)

// Stage is one named step of the pipeline.
type Stage interface {
	Name() string
	Serve(req *Request, next Next) (*Response, error)
}

// Observer sees every final (request, response, error, latency) tuple. The
// log stage and metrics are observers; they must never block.
type Observer interface {
	Observe(req *Request, resp *Response, err error, latency time.Duration)
}

// Chain is an ordered set of stages plus observers.
type Chain struct {
	stages    []Stage
	observers []Observer
}

// New validates that stages follow Order (subset, same relative order) and
// returns the chain.
func New(stages ...Stage) (*Chain, error) {
	pos := make(map[string]int, len(Order))
	for i, n := range Order {
		pos[n] = i
	}
	last := -1
	for _, s := range stages {
		p, ok := pos[s.Name()]
		if !ok {
			return nil, fmt.Errorf("pipeline: unknown stage %q", s.Name())
		}
		if p <= last {
			return nil, fmt.Errorf("pipeline: stage %q is out of order (must follow %v)", s.Name(), Order)
		}
		last = p
	}
	return &Chain{stages: stages}, nil
}

// MustNew is New that panics; for wiring code where the order is fixed.
func MustNew(stages ...Stage) *Chain {
	c, err := New(stages...)
	if err != nil {
		panic(err)
	}
	return c
}

// Observe registers observers, called in registration order.
func (c *Chain) Observe(obs ...Observer) {
	c.observers = append(c.observers, obs...)
}

// Stages returns the stage names in order.
func (c *Chain) Stages() []string {
	out := make([]string, len(c.stages))
	for i, s := range c.stages {
		out[i] = s.Name()
	}
	return out
}

// Serve runs the request through the chain and notifies observers.
func (c *Chain) Serve(req *Request) (*Response, error) {
	if req.Received.IsZero() {
		req.Received = time.Now()
	}
	if req.Ctx == nil {
		req.Ctx = context.Background()
	}
	resp, err := c.serveFrom(0, req)
	latency := time.Since(req.Received)
	for _, o := range c.observers {
		o.Observe(req, resp, err, latency)
	}
	return resp, err
}

func (c *Chain) serveFrom(i int, req *Request) (*Response, error) {
	if i >= len(c.stages) {
		return nil, ErrNoAnswer
	}
	return c.stages[i].Serve(req, func(r *Request) (*Response, error) {
		return c.serveFrom(i+1, r)
	})
}

// Reply builds a response message with the given rcode for req.
func Reply(req *Request, rcode int, status Status) *Response {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, rcode)
	return &Response{Msg: m, Status: status}
}
