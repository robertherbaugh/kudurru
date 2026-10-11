// Package blocking is the blocking stage (docs/architecture.md §7.1 step 7,
// §7.2, §7.3, D58). It filters the recursive path only: the authoritative
// stage runs earlier and answers local zones itself, so their answers
// never reach this stage and are never blocked.
//
// Per query, with the policy.Decision the identify stage attached:
//
//  1. The policy's upstreamGroup replaces the default forwarding group
//     (Request.UpstreamGroup "" or "default"); a forward zone's own group
//     is kept.
//  2. When blocking is off (blocking.enabled false, or the Toggle is
//     temporarily disabled) or the policy filters nothing, the query
//     passes.
//  3. The question is evaluated by the policy's blocking.Selection: an
//     allow list of any applied policy wins, then allow-only mode blocks,
//     then each block list's deny rules unless that list's @@ exception
//     matches.
//  4. Otherwise the query continues, and the answer is inspected: every
//     CNAME target, SVCB/HTTPS target name and A/AAAA address (and SVCB
//     address hint) is checked against the same lists (bare-IP entries
//     match addresses). Skipped when an allow list allowed the question.
//
// A block synthesises the response (BlockList response, else the policy's
// blockMode, else blocking.defaultResponse): zeroIP answers 0.0.0.0 for A
// and :: for AAAA and NODATA for other types; nxdomain answers NXDOMAIN
// without an SOA; customIP answers the configured addresses (NODATA for a
// family without one); refused answers REFUSED. The TTL is the policy's
// blockTTL, else blocking.blockTTL (10 m). When the query carried EDNS the
// answer carries EDE 15 (Blocked), or 17 (Filtered) for allow-only blocks.
// Response.Status is blocked_list, blocked_regex, blocked_exact (exact or
// suffix rules of a list made only of inline sources) or blocked_cname
// (any response-side block), and Response.List/Rule name the deciding list
// and rule for the query log.
package blocking

import (
	"math"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	engine "github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/policy"
)

// DefaultBlockTTL is the block response TTL when neither the policy nor
// the configuration sets one (D58).
const DefaultBlockTTL = 10 * time.Minute

// defaultUpstreamGroup is upstream.DefaultGroup, which a policy's
// upstreamGroup may replace.
const defaultUpstreamGroup = "default"

// ednsUDPSize is the EDNS buffer size advertised in block responses.
const ednsUDPSize = 1232

// IndexSource supplies the current policy Index for readiness and the
// kudurru_blocking_enabled gauge; policy.Source implements it.
type IndexSource interface {
	Index() *policy.Index
}

// Options configures the stage.
type Options struct {
	// Disabled turns blocking off permanently (blocking.enabled false).
	Disabled bool
	// DefaultResponse applies when neither the BlockList nor the Policy
	// sets one; empty means zeroIP.
	DefaultResponse v1alpha1.BlockResponse
	// CustomV4 and CustomV6 answer the customIP response of policies and
	// of the default (a BlockList's own customIP takes precedence).
	CustomV4, CustomV6 netip.Addr
	// BlockTTL applies when the policy sets none; zero means 10 m.
	BlockTTL time.Duration
	// AllowDegraded keeps Ready true while referenced lists are not
	// loaded.
	AllowDegraded bool
	// Toggle is the temporary disable; nil creates one.
	Toggle *Toggle
	// Index feeds Ready and the enabled gauge; optional.
	Index IndexSource
	// Registerer receives the metrics; nil leaves them unregistered.
	Registerer prometheus.Registerer
	// Now is the clock of the enabled gauge; nil means time.Now.
	Now func() time.Time
}

// OptionsFrom maps the blocking.* settings onto Options; the caller adds
// Toggle, Index and Registerer.
func OptionsFrom(c config.Blocking) Options {
	o := Options{
		Disabled:        !c.Enabled,
		DefaultResponse: v1alpha1.BlockResponse(c.DefaultResponse),
		BlockTTL:        c.BlockTTL.Std(),
		AllowDegraded:   c.AllowDegraded,
	}
	if a, err := netip.ParseAddr(c.CustomIP.V4); err == nil {
		o.CustomV4 = a.Unmap()
	}
	if a, err := netip.ParseAddr(c.CustomIP.V6); err == nil {
		o.CustomV6 = a
	}
	return o
}

// Stage implements pipeline.Stage.
type Stage struct {
	opts    Options
	live    atomic.Pointer[defaults]
	toggle  *Toggle
	metrics *metrics
}

// defaults are the settings Reconfigure replaces at runtime.
type defaults struct {
	disabled           bool
	response           v1alpha1.BlockResponse
	customV4, customV6 netip.Addr
	blockTTL           time.Duration
}

func defaultsOf(o Options) *defaults {
	d := &defaults{disabled: o.Disabled, response: o.DefaultResponse, customV4: o.CustomV4, customV6: o.CustomV6, blockTTL: o.BlockTTL}
	if d.response == "" {
		d.response = v1alpha1.BlockResponse("zeroIP")
	}
	if d.blockTTL <= 0 {
		d.blockTTL = DefaultBlockTTL
	}
	return d
}

// Reconfigure applies the blocking.* settings that are hot (enabled,
// defaultResponse, blockTTL, customIP) from o; Toggle, Index, AllowDegraded
// and the metrics stay. Queries in flight finish with the old values.
func (s *Stage) Reconfigure(o Options) { s.live.Store(defaultsOf(o)) }

// New returns the stage and registers its metrics.
func New(opts Options) (*Stage, error) {
	if opts.DefaultResponse == "" {
		opts.DefaultResponse = v1alpha1.BlockResponse("zeroIP")
	}
	if opts.BlockTTL <= 0 {
		opts.BlockTTL = DefaultBlockTTL
	}
	if opts.Toggle == nil {
		opts.Toggle = NewToggle(opts.Now)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Stage{opts: opts, toggle: opts.Toggle}
	s.live.Store(defaultsOf(opts))
	m, err := newMetrics(opts.Registerer, newEnabledCollector(s.groupState))
	if err != nil {
		return nil, err
	}
	s.metrics = m
	return s, nil
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "blocking" }

// Toggle returns the temporary disable switch.
func (s *Stage) Toggle() *Toggle { return s.toggle }

// Enabled reports whether blocking applies right now.
func (s *Stage) Enabled() bool { return !s.live.Load().disabled && s.toggle.Enabled() }

// Ready is the readiness check (checker.Register("blocking", s.Ready)):
// not ready while a referenced BlockList is not compiled yet, unless
// AllowDegraded.
func (s *Stage) Ready() (bool, []string) {
	if s.opts.AllowDegraded || s.live.Load().disabled || s.opts.Index == nil {
		return true, nil
	}
	ix := s.opts.Index.Index()
	if ix == nil {
		return false, []string{"blocking: policy index not built yet"}
	}
	missing := ix.NotLoaded()
	if len(missing) == 0 {
		return true, nil
	}
	out := make([]string, len(missing))
	for i, m := range missing {
		out[i] = "blocking: list " + m + " not loaded"
	}
	return false, out
}

func (s *Stage) groupState() map[string]bool {
	out := map[string]bool{}
	if s.opts.Index == nil {
		return out
	}
	ix := s.opts.Index.Index()
	if ix == nil {
		return out
	}
	on := s.Enabled()
	now := s.opts.Now()
	for _, g := range ix.Groups() {
		d, _ := ix.GroupDecision(g, now)
		out[g] = on && d.Filters()
	}
	return out
}

func groupLabel(req *pipeline.Request) string {
	if len(req.Client.GroupIDs) == 0 {
		return "none"
	}
	return req.Client.GroupIDs[0]
}

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	d := policy.FromRequest(req)
	if !d.Applies() {
		return next(req)
	}
	if d.UpstreamGroup != "" && (req.UpstreamGroup == "" || req.UpstreamGroup == defaultUpstreamGroup) {
		req.UpstreamGroup = d.UpstreamGroup
	}
	if !d.Filters() || req.Msg == nil || len(req.Msg.Question) == 0 {
		return next(req)
	}
	if !s.Enabled() {
		s.metrics.decision(passDisabled, groupLabel(req))
		return next(req)
	}
	q := req.Msg.Question[0]
	bd := d.Selection.Evaluate(q.Name)
	switch bd.Action {
	case engine.ActionBlock:
		return s.block(req, d, bd), nil
	case engine.ActionAllow:
		if bd.Reason == engine.ReasonAllowList {
			s.metrics.decision(allowList, groupLabel(req))
			return next(req)
		}
	}
	resp, err := next(req)
	if err != nil || resp == nil || resp.Msg == nil || resp.Msg.Rcode != dns.RcodeSuccess || len(resp.Msg.Answer) == 0 {
		s.countQuestion(req, bd)
		return resp, err
	}
	if rd := s.inspect(d, resp.Msg); rd.Action == engine.ActionBlock {
		return s.block(req, d, rd), nil
	}
	s.countQuestion(req, bd)
	return resp, err
}

func (s *Stage) countQuestion(req *pipeline.Request, bd engine.Decision) {
	if bd.Action == engine.ActionAllow {
		s.metrics.decision(allowException, groupLabel(req))
		return
	}
	s.metrics.decision(passNone, groupLabel(req))
}

// maxInspect bounds the names and addresses checked per answer.
const maxInspect = 64

// inspect applies the response-side checks to an upstream answer.
func (s *Stage) inspect(d *policy.Decision, m *dns.Msg) engine.Decision {
	var nbuf [8]string
	var abuf [16]netip.Addr
	names, addrs := nbuf[:0], abuf[:0]
	for _, rr := range m.Answer {
		if len(names)+len(addrs) >= maxInspect {
			break
		}
		switch v := rr.(type) {
		case *dns.A:
			if a, ok := netip.AddrFromSlice(v.A.To4()); ok {
				addrs = append(addrs, a)
			}
		case *dns.AAAA:
			if a, ok := netip.AddrFromSlice(v.AAAA.To16()); ok {
				addrs = append(addrs, a)
			}
		case *dns.CNAME:
			names = append(names, v.Target)
		case *dns.SVCB:
			names, addrs = svcbTargets(v, names, addrs)
		case *dns.HTTPS:
			names, addrs = svcbTargets(&v.SVCB, names, addrs)
		}
	}
	if len(names) == 0 && len(addrs) == 0 {
		return engine.Decision{}
	}
	return d.Selection.EvaluateResponse(names, addrs)
}

func svcbTargets(v *dns.SVCB, names []string, addrs []netip.Addr) ([]string, []netip.Addr) {
	if v.Target != "." && v.Target != "" {
		names = append(names, v.Target)
	}
	for _, kv := range v.Value {
		switch h := kv.(type) {
		case *dns.SVCBIPv4Hint:
			for _, ip := range h.Hint {
				if a, ok := netip.AddrFromSlice(ip.To4()); ok {
					addrs = append(addrs, a)
				}
			}
		case *dns.SVCBIPv6Hint:
			for _, ip := range h.Hint {
				if a, ok := netip.AddrFromSlice(ip.To16()); ok {
					addrs = append(addrs, a)
				}
			}
		}
	}
	return names, addrs
}

// status maps a block decision onto the status taxonomy, adding
// blocked_exact for the user's own inline lists.
func status(bd engine.Decision, info *policy.ListInfo) pipeline.Status {
	st := bd.Status()
	if st == pipeline.StatusBlockedList && info != nil && info.Inline &&
		(bd.Match.Kind == parser.KindExact || bd.Match.Kind == parser.KindSuffix) {
		return pipeline.StatusBlockedExact
	}
	return st
}

// block synthesises the block response and counts the decision.
func (s *Stage) block(req *pipeline.Request, d *policy.Decision, bd engine.Decision) *pipeline.Response {
	info := d.List(bd.List)
	group := groupLabel(req)
	switch {
	case bd.Response:
		s.metrics.decision(blockResponse, group)
	case bd.Reason == engine.ReasonAllowOnly:
		s.metrics.decision(blockAllowOnly, group)
	default:
		s.metrics.decision(blockDeny, group)
	}
	s.metrics.listHit(bd.List)

	live := s.live.Load()
	mode, v4, v6 := d.BlockMode, live.customV4, live.customV6
	if info != nil && info.Response != "" {
		mode = info.Response
		if mode == "customIP" {
			v4, v6 = info.CustomV4, info.CustomV6
		}
	}
	if mode == "" {
		mode = live.response
	}
	ttl := d.BlockTTL
	if ttl <= 0 {
		ttl = live.blockTTL
	}
	ede := dns.ExtendedErrorCodeBlocked
	if bd.Reason == engine.ReasonAllowOnly {
		ede = dns.ExtendedErrorCodeFiltered
	}
	m := Synthesize(req.Msg, Response{Mode: mode, TTL: ttl, V4: v4, V6: v6, EDE: ede, EDEText: bd.List})
	return &pipeline.Response{Msg: m, Status: status(bd, info), List: bd.List, Rule: bd.Rule()}
}

// Response describes a synthesised block response.
type Response struct {
	// Mode is zeroIP (also for unknown values), nxdomain, customIP or
	// refused.
	Mode v1alpha1.BlockResponse
	// TTL of the answer records.
	TTL time.Duration
	// V4 and V6 are the customIP answers.
	V4, V6 netip.Addr
	// EDE is the RFC 8914 info code added when the query carried EDNS;
	// EDEText its extra text (the list name). EDE 0 (Other) without text
	// adds no option.
	EDE     uint16
	EDEText string
}

// Synthesize builds the block response to q.
func Synthesize(q *dns.Msg, r Response) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(q)
	m.RecursionAvailable = true
	if len(q.Question) == 0 {
		m.Rcode = dns.RcodeFormatError
		return m
	}
	qq := q.Question[0]
	ttl := ttlSeconds(r.TTL)
	hdr := dns.RR_Header{Name: qq.Name, Rrtype: qq.Qtype, Class: qq.Qclass, Ttl: ttl}
	switch r.Mode {
	case "nxdomain":
		m.Rcode = dns.RcodeNameError
	case "refused":
		m.Rcode = dns.RcodeRefused
	case "customIP":
		switch {
		case qq.Qtype == dns.TypeA && r.V4.Is4():
			m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: r.V4.AsSlice()}}
		case qq.Qtype == dns.TypeAAAA && r.V6.Is6():
			m.Answer = []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: r.V6.AsSlice()}}
		}
	default: // zeroIP
		switch qq.Qtype {
		case dns.TypeA:
			m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: netip.IPv4Unspecified().AsSlice()}}
		case dns.TypeAAAA:
			m.Answer = []dns.RR{&dns.AAAA{Hdr: hdr, AAAA: netip.IPv6Unspecified().AsSlice()}}
		}
	}
	if opt := q.IsEdns0(); opt != nil {
		m.SetEdns0(ednsUDPSize, opt.Do())
		if r.EDE != 0 || r.EDEText != "" {
			ro := m.IsEdns0()
			ro.Option = append(ro.Option, &dns.EDNS0_EDE{InfoCode: r.EDE, ExtraText: r.EDEText})
		}
	}
	return m
}

// ttlSeconds converts d to a TTL clamped to [0, 2^31-1] (RFC 2181 §8).
func ttlSeconds(d time.Duration) uint32 {
	sec := int64(d / time.Second)
	switch {
	case sec <= 0:
		return 0
	case sec > math.MaxInt32:
		return math.MaxInt32
	}
	return uint32(sec)
}
