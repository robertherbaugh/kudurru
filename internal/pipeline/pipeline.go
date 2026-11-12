// Package pipeline defines the fixed query path: a chain of named stages
// with middleware semantics (each stage may act before and after calling
// next) and observers that see the final response (dev-docs/design-record.md
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

	"github.com/robertherbaugh/kudurru/internal/tracing"
)

// Order is the fixed stage order. "log" is implemented as an Observer
// because it must see the final response; it is listed for documentation.
var Order = []string{
	"ingress", "identify", "control", "postprocess", "local", "authoritative", "blocking",
	"rewrite", "cache", "dnssec", "upstream", "log",
}

// ErrNoAnswer is returned when every stage called next and nothing answered.
var ErrNoAnswer = errors.New("pipeline: no stage produced an answer")

// ErrDrop is returned by a stage that decided the query must not be answered
// at all. A metadata-only Response may retain its policy status, list, rule
// and cleanup ownership. With no Response, observers use StatusDropped.
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
	// Names and Tags are captured from the same immutable inventory snapshot
	// as MACs. Names are normalized DNS names; tags are provider labels, not
	// policy GroupIDs. Slices are shared and read-only.
	Names, Tags []string
	// IdentityCaptured also marks a known-empty inventory result so policy
	// matching never consults a newer mutable hostname source mid-query.
	IdentityCaptured bool
	// InventorySource binds private policy contributions to the same immutable
	// winning observation as MACs. It is never accepted from a client transport.
	InventorySource string
	// GroupIDs are the client's ClientGroups ("namespace/name", sorted),
	// set by the identify stage. The slice is shared with the policy
	// index: read it, never modify it.
	GroupIDs []string
	// TSIGKey is the lower-case, absolute name of the TSIG key that signed
	// the query, set only after the transport verified the signature (RFC
	// 8945). Queries whose signature fails verification never reach the
	// pipeline; the transport answers them NOTAUTH itself.
	TSIGKey string
	// Synthetic marks a request kudurru made itself rather than received
	// (the API's built-in dig, POST /api/v1/dns/query, which runs as any
	// client address its caller names). The log stage does not record it,
	// so it can never forge a client's query-log rows.
	Synthetic bool
}

// Request is the per-query context handed through the stages.
type Request struct {
	Ctx      context.Context
	Msg      *dns.Msg
	Client   Client
	Received time.Time
	Deadline time.Time
	// Serving holds one immutable owner cut. Synchronous subqueries borrow it;
	// work that outlives the caller must Clone and release its own lease.
	Serving ServingLease
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
	// Resolution pins the immutable resolver/validator plan for this question.
	// It is separate from any serving-configuration bundle held by a caller.
	Resolution any
	// ResponseSettings is the immutable response-processing plan captured by
	// postprocess before entering answering stages. Subqueries inherit it.
	ResponseSettings any
	// DNSSECValidation overrides the resolver default for a forward zone.
	// Nil inherits the global default. Earlier stages must not mutate it.
	DNSSECValidation *bool
	// CollectExclusions enables bounded question-rule diagnostics for HTTP dig.
	// Evidence belongs to this request, never a cached response. A copied
	// subquery replaces its own fields without mutating its parent's evidence.
	CollectExclusions   bool
	Exclusions          []RuleExclusion
	ExclusionsTruncated bool
	// Trace is the query's trace when tracing is on (D382), nil otherwise;
	// every tracing.Trace method is a no-op on nil. TraceSpan is the span
	// that work done now is a child of: the chain sets it around each
	// stage, and copies of the request (subqueries) inherit it.
	Trace     *tracing.Trace
	TraceSpan tracing.SpanRef
	// Traceparent is set when the query carried an EDNS TRACEPARENT option,
	// whoever sent it. The option itself was removed before any stage ran.
	Traceparent bool
}

// RuleExclusion identifies a disabled matching rule in the captured policy.
// It does not imply that the list or final answer is allowed.
type RuleExclusion struct {
	List   string
	Source int
	Rule   string
	Effect string
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
	Origin    ResponseOrigin
	// UpstreamContacted is set only by the upstream stage on its failure
	// response, and only when at least one server of the group was actually
	// asked (for a recursive group: the native resolver sent a query to at
	// least one authority) and failed or timed out. A refusal the node
	// produced itself before asking any server (a failed serving capture, a
	// native recursion or cache admission refused before any authority was
	// asked, a DNSSEC or policy failure) never sets it. A resolution that
	// asked an authority and then ended on an admission refusal does set it;
	// its answer is still emitted only while the retained output path admits
	// the query's dependencies and anchor. It is internal provenance (D377),
	// never derived from the wire.
	UpstreamContacted bool
	Security          Security
	Modified          bool
	CacheScope        CacheScope
	// DNSSEC is the validation state of the answer: set by recursive
	// upstream groups (and kept by the cache); unknown for everything
	// kudurru did not validate.
	DNSSEC DNSSEC
	// Transfer, when set, is a multi-message answer (an AXFR or IXFR
	// stream, RFC 5936 §2.2). Stream transports (TCP, DoT) write every
	// message it yields, in order, instead of Msg; Msg is the first of
	// them, kept for observers and for transports that cannot stream.
	// Stages set it only for requests that arrived over TCP or DoT.
	Transfer iter.Seq[*dns.Msg]
	cleanup  *responseCleanup
}

// DNSSEC is the validation state of an answer (RFC 4033 §5). The values
// match querylog.DNSSECState and recursor.Security.
type DNSSEC uint8

// DNSSEC states.
const (
	DNSSECUnknown DNSSEC = iota
	DNSSECSecure
	DNSSECInsecure
	DNSSECBogus
	DNSSECIndeterminate
)

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
	capture   Capture
	tracer    *tracing.Tracer
	privacy   TracePrivacy
}

// TracePrivacy reports which query facts a kept trace may export: the
// question name and the client address. It follows querylog.privacy, so a
// trace never shows more than the query log may store.
type TracePrivacy func() (names, clients bool)

// SetTracer turns on query tracing (D382). Configure it once, before
// listeners start. A nil privacy exports neither names nor clients.
func (c *Chain) SetTracer(t *tracing.Tracer, privacy TracePrivacy) {
	c.tracer, c.privacy = t, privacy
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
func (c *Chain) Serve(req *Request) (resp *Response, err error) {
	if req.Received.IsZero() {
		req.Received = time.Now()
	}
	if req.Ctx == nil {
		req.Ctx = context.Background()
	}
	// Only the chain the transport calls starts a trace; a dependency
	// chain entered with a traced request adds its stage spans to it.
	var trace *tracing.Trace
	if c.tracer != nil && req.Trace == nil && req.Msg != nil && req.Msg.Opcode == dns.OpcodeQuery {
		// The TRACEPARENT option is removed from every query. It joins the
		// caller's trace only from a peer whose PROXYv2 header passed the
		// proxy policy (exact peer and shared-secret TLV, D45/D56/D72),
		// the same trust the client address itself rides on.
		trusted := req.Client.ProxiedFrom.IsValid() && !req.Client.Synthetic
		trace, req.Traceparent = c.tracer.Begin(req.Msg, trusted, req.Received)
		req.Trace, req.TraceSpan = trace, tracing.Root
		defer func() {
			if req.Trace == trace {
				req.Trace, req.TraceSpan = nil, 0
			}
		}()
	}
	var owned ServingLease
	completed := false
	defer func() {
		if !completed {
			resp.Close() // an observer panic must not strand response ownership
		}
		if owned != nil {
			owned.Release()
			req.Serving = nil
		}
	}()
	if req.Serving == nil && c.capture != nil {
		owned, err = c.capture(req)
		if err == nil && owned == nil {
			err = ErrServingCapture
		}
		if err == nil {
			req.Serving = owned
		}
	}
	if err == nil {
		resp, err = c.serveFrom(0, req)
	}
	latency := time.Since(req.Received)
	if trace != nil {
		c.finishTrace(trace, req, resp, err, latency)
	}
	for _, o := range c.observers {
		o.Observe(req, resp, err, latency)
	}
	if err != nil {
		resp.Close()
	} else if resp != nil && resp.Transfer != nil && owned != nil {
		resp.AddCleanup(owned.Release)
		owned = nil // the transport now owns stream cleanup
	}
	completed = true
	return resp, err
}

func (c *Chain) serveFrom(i int, req *Request) (*Response, error) {
	if i >= len(c.stages) {
		return nil, ErrNoAnswer
	}
	if req.Trace != nil {
		return c.serveTraced(i, req)
	}
	return c.stages[i].Serve(req, func(r *Request) (*Response, error) {
		return c.serveFrom(i+1, r)
	})
}

// serveTraced runs stage i inside its own span, a child of the span the
// request carries. Stages are middleware, so a stage's span contains the
// spans of the stages it called next for.
func (c *Chain) serveTraced(i int, req *Request) (*Response, error) {
	stage := c.stages[i]
	parent := req.TraceSpan
	span := req.Trace.Start(parent, stageSpanNames[stage.Name()], tracing.KindInternal, time.Now())
	req.TraceSpan = span
	resp, err := stage.Serve(req, func(r *Request) (*Response, error) {
		return c.serveFrom(i+1, r)
	})
	req.TraceSpan = parent
	failure := ""
	switch {
	case err != nil && !errors.Is(err, ErrNoAnswer) && !errors.Is(err, ErrDrop):
		failure = "stage error"
	case err == nil && resp != nil && resp.Status.failed():
		failure = resp.Status.String()
	}
	if resp != nil && failure == "" && span != 0 {
		req.Trace.SetStr(span, "kudurru.status", resp.Status.String())
	}
	req.Trace.End(span, time.Now(), failure)
	return resp, err
}

// stageSpanNames are the span names of the stages; constant strings, so a
// traced stage allocates no name.
var stageSpanNames = func() map[string]string {
	m := make(map[string]string, len(Order))
	for _, n := range Order {
		m[n] = "kudurru." + n
	}
	return m
}()

// failed reports the statuses that make a trace kept by the tail rule
// (D382): the answer is SERVFAIL because resolution failed (upstream
// failure or timeout, DNSSEC bogus, an unsynced zone). Every other status
// is not a failure even with a SERVFAIL rcode (the drain answer for the
// health names, a cached SERVFAIL); refusals, rate-limit and control
// drops are policy, and a flood of them must not turn into a flood of
// exported traces.
func (s Status) failed() bool {
	switch s {
	case StatusServfailUpstream, StatusTimeoutUpstream, StatusDNSSECBogus, StatusServfailUnsynced:
		return true
	}
	return false
}

// finishTrace decides whether the query's trace is kept, adds the root
// attributes a kept trace exports and hands it to the tracer.
func (c *Chain) finishTrace(t *tracing.Trace, req *Request, resp *Response, err error, latency time.Duration) {
	// The status decides, not the rcode: the ingress stage answers the
	// health names SERVFAIL (StatusSpecialDomain) while the node drains,
	// and a cached SERVFAIL is a cache hit; neither is a failure to keep.
	dropped := errors.Is(err, ErrDrop)
	failed := (err != nil && !dropped) || (resp != nil && resp.Status.failed())
	if t.Decide(latency, failed) {
		q := req.Question()
		names, clients := false, false
		if c.privacy != nil {
			names, clients = c.privacy()
		}
		if names {
			t.SetStr(tracing.Root, "dns.question.name", q.Name)
		}
		if clients && req.Client.Addr.IsValid() {
			t.SetStr(tracing.Root, "client.address", req.Client.Addr.Addr().String())
		}
		t.SetStr(tracing.Root, "dns.question.type", dns.TypeToString[q.Qtype])
		t.SetStr(tracing.Root, "network.transport", req.Client.Proto.String())
		status := StatusUnknown
		if resp != nil {
			status = resp.Status
			if resp.Msg != nil {
				t.SetStr(tracing.Root, "dns.response.code", dns.RcodeToString[resp.Msg.Rcode])
			}
		} else if err != nil {
			status = StatusDropped
		}
		t.SetStr(tracing.Root, "kudurru.status", status.String())
		if resp != nil && resp.Upstream != "" {
			t.SetStr(tracing.Root, "kudurru.upstream", resp.Upstream)
		}
		if failed {
			t.End(tracing.Root, req.Received.Add(latency), status.String())
		} else {
			t.End(tracing.Root, req.Received.Add(latency), "")
		}
	}
	t.Finish(req.Received.Add(latency))
}

// Reply builds a response message with the given rcode for req.
func Reply(req *Request, rcode int, status Status) *Response {
	m := new(dns.Msg)
	m.SetRcode(req.Msg, rcode)
	return &Response{Msg: m, Status: status}
}
