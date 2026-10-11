// Package control is the global-rules stage (docs/architecture.md §7.1
// step 4). It runs after identify and before any answer is produced, and
// decides one of three things per query:
//
//   - Allow: continue down the pipeline.
//   - DropSilently: malformed queries (QR set, qtype 0 or OPT, qclass 0, a
//     question name that is not a domain name) get no answer at all. The
//     stage returns pipeline.ErrDrop with a nil Response; the log observer
//     records StatusDropped.
//   - Refuse: clients outside Options.AllowedNetworks and queries for a
//     type in Options.BlockedQTypes are answered REFUSED with
//     StatusRefusedACL and, when the query carried EDNS, EDE 18
//     (Prohibited).
//
// The rules come from configuration today. Per-policy rules (v0.2) read
// the policy.Decision that identify attached to the request.
package control

import (
	"net/netip"
	"slices"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// ednsUDPSize is the EDNS buffer size advertised in refusals (DNS flag
// day 2020).
const ednsUDPSize = 1232

// Options are the global rules.
type Options struct {
	// AllowedNetworks, when non-empty, limits who may query: other clients
	// are refused. Loopback clients are always allowed so the local
	// self-checks and tools keep working.
	AllowedNetworks []netip.Prefix
	// BlockedQTypes are refused for every client (for example ANY-like
	// meta types or HINFO).
	BlockedQTypes []uint16
	// KeepMalformed answers malformed queries normally instead of dropping
	// them (debugging only).
	KeepMalformed bool
}

// Action is the stage's verdict.
type Action uint8

// Actions.
const (
	Allow Action = iota
	DropSilently
	Refuse
)

func (a Action) String() string {
	switch a {
	case Allow:
		return "allow"
	case DropSilently:
		return "drop"
	case Refuse:
		return "refuse"
	}
	return "unknown"
}

// Stage implements pipeline.Stage.
type Stage struct {
	opts Options
}

// New returns the stage. Prefixes are masked; invalid ones are ignored.
func New(opts Options) *Stage {
	nets := make([]netip.Prefix, 0, len(opts.AllowedNetworks))
	for _, p := range opts.AllowedNetworks {
		if !p.IsValid() {
			continue
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		nets = append(nets, p.Masked())
	}
	opts.AllowedNetworks = nets
	opts.BlockedQTypes = slices.Clone(opts.BlockedQTypes)
	return &Stage{opts: opts}
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return "control" }

// Decide applies the rules to req.
func (s *Stage) Decide(req *pipeline.Request) Action {
	m := req.Msg
	if m == nil {
		return DropSilently
	}
	if !s.opts.KeepMalformed && malformed(m) {
		return DropSilently
	}
	if len(s.opts.AllowedNetworks) > 0 && !s.allowed(req.Client.Addr.Addr()) {
		return Refuse
	}
	if len(s.opts.BlockedQTypes) > 0 && len(m.Question) > 0 && slices.Contains(s.opts.BlockedQTypes, m.Question[0].Qtype) {
		return Refuse
	}
	return Allow
}

func (s *Stage) allowed(a netip.Addr) bool {
	if !a.IsValid() {
		return false
	}
	a = a.Unmap()
	if a.IsLoopback() {
		return true
	}
	for _, p := range s.opts.AllowedNetworks {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// malformed reports queries that deserve no answer. Opcode, question
// count and EDNS version are already checked by ingress.
func malformed(m *dns.Msg) bool {
	if m.Response {
		return true
	}
	if len(m.Question) != 1 {
		return true
	}
	q := m.Question[0]
	if q.Qtype == 0 || q.Qtype == dns.TypeOPT || q.Qclass == 0 {
		return true
	}
	_, ok := dns.IsDomainName(q.Name)
	return !ok || !dns.IsFqdn(q.Name)
}

// Serve implements pipeline.Stage.
func (s *Stage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	switch s.Decide(req) {
	case DropSilently:
		return nil, pipeline.ErrDrop
	case Refuse:
		r := pipeline.Reply(req, dns.RcodeRefused, pipeline.StatusRefusedACL)
		if opt := req.Msg.IsEdns0(); opt != nil {
			r.Msg.SetEdns0(ednsUDPSize, opt.Do())
			ro := r.Msg.IsEdns0()
			ro.Option = append(ro.Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeProhibited})
		}
		return r, nil
	}
	return next(req)
}
