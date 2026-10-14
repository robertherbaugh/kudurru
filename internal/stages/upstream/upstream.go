// Package upstream is the terminal forwarding stage (dev-docs/design-record.md
// §7.1 step 11). It forwards over the groups of internal/upstream: the
// group is Request.UpstreamGroup when the authoritative stage set it, else
// the longest conditional-forwarding suffix, else "default". Answers from
// the default group are Cacheable; conditional ones are not (blocky rule).
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Stage implements pipeline.Stage.
type Stage struct {
	state atomic.Pointer[resolutionState]
	// Capture supplies the immutable route/trust cut held by Request.Serving.
	Capture func(*pipeline.Request) (*View, error)

	// Group, Servers and Timeout describe the group built by the
	// convenience constructor New, for startup logging; they are empty for
	// NewStage. Set().Status() reports every group of either kind.
	Group   string
	Servers []string
	Timeout time.Duration
}

type resolutionState struct {
	set       *upstream.Set
	validator *dnssec.Validator
	enabled   bool
	epoch     uint64
}

var nextEpoch atomic.Uint64

// NewStage returns a stage forwarding over set. set must not be nil.
func NewStage(set *upstream.Set) *Stage {
	s := &Stage{}
	s.state.Store(&resolutionState{set: set, epoch: nextEpoch.Add(1)})
	return s
}

// New builds a stage with a single default group of servers using the
// default strategy (parallel_best), for callers and tests that only have a
// server list; group must be "default". Production wiring builds the full
// configuration with upstream.New(upstream.ConfigFrom(cfg.Upstreams), ...)
// and passes it to NewStage.
func New(group string, servers []string, timeout time.Duration) (*Stage, error) {
	if group != upstream.DefaultGroup {
		return nil, fmt.Errorf("upstream: the convenience constructor builds the %q group, not %q", upstream.DefaultGroup, group)
	}
	if len(servers) == 0 {
		return nil, errors.New("upstream: at least one server is required")
	}
	if timeout <= 0 {
		timeout = upstream.DefaultTimeout
	}
	set, err := upstream.New(upstream.Config{Groups: map[string]upstream.GroupConfig{
		group: {Servers: servers, Timeout: timeout},
	}}, upstream.Options{})
	if err != nil {
		return nil, err
	}
	s := NewStage(set)
	s.Group, s.Servers, s.Timeout = group, append([]string(nil), servers...), timeout
	return s, nil
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "upstream" }

// Set returns the current upstream set.
func (s *Stage) Set() *upstream.Set { return s.state.Load().set }

// Swap installs next (a configuration reload) and returns the previous
// set. Retire it with prev.CloseReplacedBy(next) after a grace period of
// one query timeout so that exchanges in flight on it can finish.
func (s *Stage) Swap(next *upstream.Set) (prev *upstream.Set) {
	for {
		old := s.state.Load()
		n := *old
		n.set, n.epoch = next, nextEpoch.Add(1)
		if s.state.CompareAndSwap(old, &n) {
			return old.set
		}
	}
}

// Replace atomically publishes route and trust settings as one immutable
// resolution state. The caller retires the returned set after in-flight
// queries have finished. Invalid input leaves the old publication intact.
func (s *Stage) Replace(set *upstream.Set, v *dnssec.Validator, enabled bool) (*upstream.Set, error) {
	if set == nil || enabled && v == nil {
		return nil, fmt.Errorf("upstream: replacement requires a set and enabled validation requires a validator")
	}
	n := &resolutionState{set: set, validator: v, enabled: enabled, epoch: nextEpoch.Add(1)}
	return s.state.Swap(n).set, nil
}

// SetValidation publishes operator-managed trust/NTA configuration with
// its opt-in global default. Every replacement fences outstanding fetches.
func (s *Stage) SetValidation(v *dnssec.Validator, enabled bool) error {
	if enabled && v == nil {
		return fmt.Errorf("upstream: enabled validation requires a validator")
	}
	for {
		old := s.state.Load()
		n := *old
		n.validator, n.enabled, n.epoch = v, enabled, nextEpoch.Add(1)
		if s.state.CompareAndSwap(old, &n) {
			return nil
		}
	}
}

// Bind pins the plan before cache lookup, including when caching is disabled.
// Existing request plans derive alias questions under their original snapshot.
func (s *Stage) Bind(req *pipeline.Request) (*upstream.QueryPlan, error) {
	if p, ok := req.Resolution.(*upstream.QueryPlan); ok {
		n, err := p.Derive(req)
		if err == nil {
			req.Resolution = n
		}
		return n, err
	}
	state := s.state.Load()
	if s.Capture != nil {
		view, err := s.Capture(req)
		if err != nil {
			return nil, err
		}
		if view == nil {
			return nil, pipeline.ErrServingCapture
		}
		p, err := view.set.Plan(req, view.validator, view.enabled, view.epoch, view.current)
		if err == nil {
			req.Resolution = p
		}
		return p, err
	}
	if req.Serving != nil {
		return nil, pipeline.ErrServingCapture
	}
	p, err := state.set.Plan(req, state.validator, state.enabled, state.epoch, func() bool { return s.state.Load() == state })
	if err == nil {
		req.Resolution = p
	}
	return p, err
}

// ECSSubnet is the client subnet the stage would send for m from client;
// the cache stage keys entries by it.
func (s *Stage) ECSSubnet(m *dns.Msg, client netip.Addr) netip.Prefix {
	return s.Set().ECSSubnet(m, client)
}

// Serve implements pipeline.Stage. It never calls next.
func (s *Stage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	plan, err := s.Bind(req)
	if err != nil {
		return s.failure(req, req.UpstreamGroup, err), nil
	}

	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if !req.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, req.Deadline)
		defer cancel()
	}
	res, err := plan.Exchange(ctx, req.Msg)
	if err != nil {
		return s.failure(req, plan.Group(), err), nil
	}
	// DNSSEC proof material stays intact until validation and raw cache storage.
	if !plan.Validates() && !plan.RawResponse() {
		Finalize(req.Msg, res.Msg, pipeline.Security{}, false, time.Now())
	}
	return &pipeline.Response{
		Msg:        res.Msg,
		Status:     pipeline.StatusForwarded,
		Upstream:   res.Upstream,
		Cacheable:  plan.Cacheable(),
		Origin:     pipeline.OriginForwarded,
		CacheScope: plan.Scope(),
		DNSSEC:     pipeline.DNSSEC(res.DNSSEC), // same values
	}, nil
}

// Finalize personalizes flags and DNSSEC representation on a private message.
// The validator/cache invoke it only after raw insertion; outer synthesis
// stages must clear AD on any material modification.
func Finalize(req, resp *dns.Msg, sec pipeline.Security, modified bool, now time.Time) {
	upstream.FinishReply(req, resp, -1)
	resp.CheckingDisabled = req.CheckingDisabled
	do := req.IsEdns0() != nil && req.IsEdns0().Do()
	resp.AuthenticatedData = sec.State == pipeline.ValidationSecure && sec.AvailableAt(now) && !modified && !req.CheckingDisabled && (do || req.AuthenticatedData)
	if !do {
		qt := uint16(0)
		if len(req.Question) > 0 {
			qt = req.Question[0].Qtype
		}
		for _, part := range []*[]dns.RR{&resp.Answer, &resp.Ns, &resp.Extra} {
			out := (*part)[:0]
			for _, rr := range *part {
				t := rr.Header().Rrtype
				if (t == dns.TypeRRSIG || t == dns.TypeNSEC || t == dns.TypeNSEC3 || t == dns.TypeDNSKEY || t == dns.TypeDS || t == dns.TypeNSEC3PARAM) && t != qt {
					continue
				}
				out = append(out, rr)
			}
			*part = out
		}
	}
}

// failure answers SERVFAIL: the upstream's own SERVFAIL when one arrived
// (it may carry an extended error), else a synthesised one with EDE 22 (no
// reachable authority) for timeouts or 23 (network error).
func (s *Stage) failure(req *pipeline.Request, group string, err error) *pipeline.Response {
	status := pipeline.StatusServfailUpstream
	if upstream.IsTimeout(err) {
		status = pipeline.StatusTimeoutUpstream
	}
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Msg != nil {
		m := ue.Msg
		m.Rcode = dns.RcodeServerFailure
		m.Answer, m.Ns = nil, nil
		upstream.FinishReply(req.Msg, m, -1)
		r := &pipeline.Response{Msg: m, Status: status, Upstream: ue.Upstream}
		if dnssecFailure(m) {
			r.Status, r.DNSSEC = pipeline.StatusDNSSECBogus, pipeline.DNSSECBogus
		}
		return r
	}
	r := pipeline.Reply(req, dns.RcodeServerFailure, status)
	r.Upstream = group
	r.Msg.RecursionAvailable = true
	upstream.FinishReply(req.Msg, r.Msg, -1)
	code := uint16(dns.ExtendedErrorCodeNetworkError)
	if status == pipeline.StatusTimeoutUpstream {
		code = dns.ExtendedErrorCodeNoReachableAuthority
	}
	upstream.AddEDE(r.Msg, code, "")
	return r
}

// dnssecFailure reports whether a SERVFAIL carries an extended error that
// blames DNSSEC validation (RFC 8914 codes 6 to 12): the recursive groups'
// bogus answers, and validating forwarders that say why.
func dnssecFailure(m *dns.Msg) bool {
	opt := m.IsEdns0()
	if opt == nil {
		return false
	}
	for _, o := range opt.Option {
		if e, ok := o.(*dns.EDNS0_EDE); ok {
			switch e.InfoCode {
			case dns.ExtendedErrorCodeDNSBogus, dns.ExtendedErrorCodeSignatureExpired,
				dns.ExtendedErrorCodeSignatureNotYetValid, dns.ExtendedErrorCodeDNSKEYMissing,
				dns.ExtendedErrorCodeRRSIGsMissing, dns.ExtendedErrorCodeNoZoneKeyBitSet,
				dns.ExtendedErrorCodeNSECMissing:
				return true
			}
		}
	}
	return false
}
