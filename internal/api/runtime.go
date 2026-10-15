package api

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
)

// BlockingState is GET|POST /dns/blocking.
type BlockingState struct {
	// Blocking is true while blocking is on.
	Blocking bool `json:"blocking"`
	// Timer is the number of seconds until blocking comes back; absent
	// while enabled or disabled indefinitely.
	Timer *float64 `json:"timer,omitempty"`
	// Until is when blocking comes back; absent while enabled or disabled
	// indefinitely.
	Until *time.Time `json:"until,omitempty"`
	// Configured is blocking.enabled from the settings; when false the
	// toggle has no effect.
	Configured bool `json:"configured"`
	// Scope is "node" while the toggle is not replicated (the Publisher
	// seam), "cluster" once it is.
	Scope string `json:"scope" enum:"node,cluster"`
	Node  string `json:"node"`
	Took
}

type blockingOutput struct {
	Body *BlockingState
}

type setBlockingInput struct {
	Body struct {
		Blocking bool `json:"blocking"`
		// Timer disables blocking for this many seconds (with blocking
		// false); 0 or absent disables it until enabled again.
		Timer *float64 `json:"timer,omitempty" minimum:"0"`
		// Until disables blocking until this instant (RFC 3339).
		Until *time.Time `json:"until,omitempty"`
	}
}

type flushInput struct {
	Body *struct {
		// Name flushes only this name (every common type, with and without
		// DO/CD); absent flushes the whole cache.
		Name string `json:"name,omitempty"`
	}
	Name string `query:"name" doc:"Same as the body field."`
}

// FlushResult is the outcome of POST /cache/flush.
type FlushResult struct {
	Flushed int    `json:"flushed"`
	Name    string `json:"name,omitempty"`
	Node    string `json:"node"`
	Took
}

type flushOutput struct {
	Body *FlushResult
}

// DNSQueryRequest is the body of POST /dns/query (the built-in dig).
type DNSQueryRequest struct {
	Name string `json:"name" minLength:"1" maxLength:"255"`
	// Type is the query type mnemonic (default A).
	Type string `json:"type,omitempty"`
	// Client is the source address the pipeline sees (policies, client
	// groups); default 127.0.0.1.
	Client string `json:"client,omitempty"`
	// Proto is the transport the pipeline sees (default udp).
	Proto string `json:"proto,omitempty" enum:"udp,tcp,dot,doh,doq,doh3"`
	// DNSSEC sets the DO bit.
	DNSSEC bool `json:"dnssec,omitempty"`
}

// DNSQueryResult is the answer of POST /dns/query.
type DNSQueryResult struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Client      string   `json:"client"`
	Rcode       string   `json:"rcode"`
	Status      string   `json:"status"`
	StatusClass string   `json:"status_class"`
	Upstream    string   `json:"upstream,omitempty"`
	List        string   `json:"list,omitempty"`
	Rule        string   `json:"rule,omitempty"`
	Answer      []string `json:"answer" nullable:"false"`
	Authority   []string `json:"authority" nullable:"false"`
	Additional  []string `json:"additional" nullable:"false"`
	Flags       DNSFlags `json:"flags"`
	// LatencyMS is the pipeline latency.
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
	Node      string  `json:"node"`
	Took
}

// DNSFlags are the header flags of the answer.
type DNSFlags struct {
	AA bool `json:"aa"`
	TC bool `json:"tc"`
	RD bool `json:"rd"`
	RA bool `json:"ra"`
	AD bool `json:"ad"`
	CD bool `json:"cd"`
}

type dnsQueryInput struct {
	Body DNSQueryRequest
}

type dnsQueryOutput struct {
	Body *DNSQueryResult
}

func (s *Server) registerRuntime() {
	register(s, operation(http.MethodGet, "/dns/blocking", "getBlocking", "Blocking state of this node", "runtime"),
		perm{resource: "blocking", verb: "read"}, s.getBlocking)
	register(s, operation(http.MethodPost, "/dns/blocking", "setBlocking", "Enable blocking, or disable it for a while", "runtime"),
		perm{resource: "blocking", verb: "toggle"}, s.setBlocking)
	register(s, operation(http.MethodPost, "/cache/flush", "flushCache", "Flush the response cache (or one name)", "runtime"),
		perm{resource: "cache", verb: "flush"}, s.flushCache)
	register(s, operation(http.MethodPost, "/dns/query", "dnsQuery", "Run one query through the pipeline in-process (built-in dig)", "runtime"),
		perm{resource: "stats", verb: "read"}, s.dnsQuery)
}

func (s *Server) blockingState() *BlockingState {
	out := &BlockingState{Blocking: true, Scope: "node", Node: s.opts.NodeName, Configured: s.config().Blocking.Enabled}
	if s.opts.Blocking == nil {
		out.Blocking = out.Configured
		return out
	}
	t := s.opts.Blocking.Toggle()
	out.Blocking = s.opts.Blocking.Enabled()
	if until := t.DisabledUntil(); !until.IsZero() && until.Before(blockingstage.Forever) {
		u := until.UTC()
		out.Until = &u
		secs := until.Sub(s.opts.Now()).Seconds()
		if secs < 0 {
			secs = 0
		}
		out.Timer = &secs
	}
	return out
}

func (s *Server) getBlocking(ctx context.Context, _ *struct{}) (*blockingOutput, error) {
	if s.opts.BlockingReads != nil {
		if ctx == nil || ctx.Err() != nil {
			return nil, errUnavailable("original blocking state unavailable")
		}
		now := s.opts.Now()
		if now.IsZero() {
			return nil, errUnavailable("original blocking observation time unavailable")
		}
		configured, enabled, until, err := s.opts.BlockingReads.BlockingState(ctx, now)
		if err != nil || ctx.Err() != nil || enabled && !configured {
			return nil, errUnavailable("original blocking state unavailable")
		}
		out := &BlockingState{Configured: configured, Blocking: enabled, Scope: "cluster", Node: s.opts.NodeName}
		if configured && !enabled && until.After(now) && until.Before(blockingstage.Forever) {
			u, seconds := until.UTC(), until.Sub(now).Seconds()
			out.Until, out.Timer = &u, &seconds
		}
		return &blockingOutput{Body: out}, nil
	}
	return &blockingOutput{Body: s.blockingState()}, nil
}

func (s *Server) setBlocking(ctx context.Context, in *setBlockingInput) (*blockingOutput, error) {
	if s.opts.Blocking == nil {
		return nil, errNotImplemented("the blocking toggle")
	}
	before := s.blockingState()
	t := s.opts.Blocking.Toggle()
	var err error
	switch {
	case in.Body.Blocking:
		err = t.Enable()
	case in.Body.Until != nil:
		d := in.Body.Until.Sub(s.opts.Now())
		if d <= 0 {
			return nil, errInvalid("until: %s is in the past", in.Body.Until.Format(time.RFC3339))
		}
		_, err = t.Disable(d)
	case in.Body.Timer != nil && *in.Body.Timer > 0:
		_, err = t.Disable(time.Duration(*in.Body.Timer * float64(time.Second)))
	default:
		_, err = t.Disable(0)
	}
	after := s.blockingState()
	s.recordAction(ctx, "blocking.toggle", audit.Resource{Kind: "blocking"}, err, before, after)
	if err != nil {
		// The local change applied; replication failed (the Publisher).
		s.log.Warn("blocking toggle not replicated", "err", err)
	}
	return &blockingOutput{Body: after}, nil
}

// flushTypes are the query types a by-name flush removes.
var flushTypes = []uint16{
	dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeTXT, dns.TypeNS, dns.TypeSOA, dns.TypePTR,
	dns.TypeSRV, dns.TypeHTTPS, dns.TypeSVCB, dns.TypeCAA, dns.TypeDS, dns.TypeDNSKEY, dns.TypeANY, dns.TypeNAPTR,
}

func (s *Server) flushCache(ctx context.Context, in *flushInput) (*flushOutput, error) {
	if s.opts.ResponseCache == nil {
		return nil, errNotImplemented("the response cache")
	}
	name := in.Name
	if in.Body != nil && in.Body.Name != "" {
		name = in.Body.Name
	}
	var flushed int
	if name == "" {
		before := s.opts.ResponseCache.Len()
		s.opts.ResponseCache.Flush()
		flushed = max(before-s.opts.ResponseCache.Len(), 0)
	} else {
		if _, ok := dns.IsDomainName(dns.Fqdn(strings.TrimSpace(name))); !ok {
			return nil, errInvalid("name: %q is not a domain name", name)
		}
		flushed = s.flushName(name)
	}
	s.recordAction(ctx, "cache.flush", audit.Resource{Kind: "cache", Name: name}, nil, nil, map[string]any{"flushed": flushed})
	return &flushOutput{Body: &FlushResult{Flushed: flushed, Name: name, Node: s.opts.NodeName}}, nil
}

var protoByName = map[string]pipeline.Proto{
	"": pipeline.ProtoUDP, "udp": pipeline.ProtoUDP, "tcp": pipeline.ProtoTCP, "dot": pipeline.ProtoDoT,
	"doh": pipeline.ProtoDoH, "doq": pipeline.ProtoDoQ, "doh3": pipeline.ProtoDoH3,
}

func (s *Server) dnsQuery(ctx context.Context, in *dnsQueryInput) (*dnsQueryOutput, error) {
	if s.opts.Chain == nil {
		return nil, errNotImplemented("the DNS pipeline")
	}
	req := in.Body
	qtype := uint16(dns.TypeA)
	if req.Type != "" {
		t, ok := dns.StringToType[strings.ToUpper(strings.TrimSpace(req.Type))]
		if !ok {
			return nil, errInvalid("type: unknown query type %q", req.Type)
		}
		qtype = t
	}
	name := dns.Fqdn(strings.TrimSpace(req.Name))
	if _, ok := dns.IsDomainName(name); !ok {
		return nil, errInvalid("name: %q is not a domain name", req.Name)
	}
	client := netip.MustParseAddr("127.0.0.1")
	if req.Client != "" {
		a, err := netip.ParseAddr(req.Client)
		if err != nil {
			return nil, errInvalid("client: %q is not an IP address", req.Client)
		}
		client = a.Unmap()
	}
	proto, ok := protoByName[req.Proto]
	if !ok {
		return nil, errInvalid("proto: unknown transport %q", req.Proto)
	}
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = true
	m.SetEdns0(1232, req.DNSSEC)
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	preq := &pipeline.Request{
		Ctx: qctx, Msg: m, Received: start, Deadline: start.Add(5 * time.Second),
		// Synthetic: the caller chooses the client address, so the log
		// stage must not record this as that client's traffic.
		Client: pipeline.Client{Addr: netip.AddrPortFrom(client, 0), Proto: proto, ClientID: "api-dig", Synthetic: true},
	}
	resp, err := s.opts.Chain.Serve(preq)
	defer func() { resp.Close() }() // nil-safe, including error and unused stream responses
	out := &DNSQueryResult{
		Name: name, Type: dns.TypeToString[qtype], Client: client.String(), Node: s.opts.NodeName,
		Answer: []string{}, Authority: []string{}, Additional: []string{},
		LatencyMS: float64(time.Since(start).Microseconds()) / 1000,
	}
	if err != nil {
		out.Rcode = dns.RcodeToString[dns.RcodeServerFailure]
		out.Status = pipeline.StatusServfailUpstream.String()
		out.StatusClass = string(pipeline.StatusServfailUpstream.Class())
		out.Error = err.Error()
		if errors.Is(err, pipeline.ErrDrop) {
			out.Status, out.Rcode = pipeline.StatusDropped.String(), "none"
			out.StatusClass = string(pipeline.StatusDropped.Class())
			if resp != nil {
				out.Status, out.StatusClass = resp.Status.String(), string(resp.Status.Class())
				out.Upstream, out.List, out.Rule = resp.Upstream, resp.List, resp.Rule
			}
		}
		return &dnsQueryOutput{Body: out}, nil
	}
	if resp == nil || resp.Msg == nil {
		out.Error = "the pipeline returned no answer"
		return &dnsQueryOutput{Body: out}, nil
	}
	out.Status, out.StatusClass = resp.Status.String(), string(resp.Status.Class())
	out.Upstream, out.List, out.Rule = resp.Upstream, resp.List, resp.Rule
	r := resp.Msg
	out.Rcode = dns.RcodeToString[r.Rcode]
	out.Flags = DNSFlags{AA: r.Authoritative, TC: r.Truncated, RD: r.RecursionDesired, RA: r.RecursionAvailable, AD: r.AuthenticatedData, CD: r.CheckingDisabled}
	for _, rr := range r.Answer {
		out.Answer = append(out.Answer, rr.String())
	}
	for _, rr := range r.Ns {
		out.Authority = append(out.Authority, rr.String())
	}
	for _, rr := range r.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}
		out.Additional = append(out.Additional, rr.String())
	}
	return &dnsQueryOutput{Body: out}, nil
}
