// Package upstream is the terminal forwarding stage (docs/architecture.md
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

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Stage implements pipeline.Stage.
type Stage struct {
	set atomic.Pointer[upstream.Set]

	// Group, Servers and Timeout describe the group built by the
	// convenience constructor New, for startup logging; they are empty for
	// NewStage. Set().Status() reports every group of either kind.
	Group   string
	Servers []string
	Timeout time.Duration
}

// NewStage returns a stage forwarding over set. set must not be nil.
func NewStage(set *upstream.Set) *Stage {
	s := &Stage{}
	s.set.Store(set)
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
func (s *Stage) Set() *upstream.Set { return s.set.Load() }

// Swap installs next (a configuration reload) and returns the previous
// set. Retire it with prev.CloseReplacedBy(next) after a grace period of
// one query timeout so that exchanges in flight on it can finish.
func (s *Stage) Swap(next *upstream.Set) (prev *upstream.Set) {
	return s.set.Swap(next)
}

// ECSSubnet is the client subnet the stage would send for m from client;
// the cache stage keys entries by it.
func (s *Stage) ECSSubnet(m *dns.Msg, client netip.Addr) netip.Prefix {
	return s.set.Load().ECSSubnet(m, client)
}

// Serve implements pipeline.Stage. It never calls next.
func (s *Stage) Serve(req *pipeline.Request, _ pipeline.Next) (*pipeline.Response, error) {
	set := s.set.Load()
	q := req.Question()
	group, conditional, err := set.Route(q.Name, req.UpstreamGroup)
	if err != nil {
		return s.failure(req, group, err), nil
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
	res, err := set.Exchange(ctx, group, set.PrepareQuery(req.Msg, req.Client.Addr.Addr()))
	if err != nil {
		return s.failure(req, group, err), nil
	}
	upstream.FinishReply(req.Msg, res.Msg, -1)
	return &pipeline.Response{
		Msg:       res.Msg,
		Status:    pipeline.StatusForwarded,
		Upstream:  res.Upstream,
		Cacheable: !conditional,
		DNSSEC:    pipeline.DNSSEC(res.DNSSEC), // same values
	}, nil
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
