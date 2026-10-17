package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"net/netip"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// QueryPlan pins raw resolution and validation semantics. It is immutable;
// derivations retain the same resolver/validator even during a reload.
type QueryPlan struct {
	set            *Set
	validator      *dnssec.Validator
	validate       bool
	client         netip.Addr
	group          string
	conditional    bool
	scope          pipeline.CacheScope
	active         [32]byte
	current        func() bool
	defaultEnabled bool
	raw            bool
}

func (s *Set) Identity() [32]byte { return s.identity }

// Plan captures all inputs before a cache lookup. current reports whether
// this configuration publication is still eligible to insert cache data.
func (s *Set) Plan(req *pipeline.Request, v *dnssec.Validator, enabled bool, epoch uint64, current func() bool) (*QueryPlan, error) {
	p := &QueryPlan{set: s, validator: v, defaultEnabled: enabled, client: req.Client.Addr.Addr(), current: current}
	if v != nil {
		p.active = v.ActiveContextID()
	}
	p.scope.Epoch = epoch
	return p.Derive(req)
}

func (p *QueryPlan) Derive(req *pipeline.Request) (*QueryPlan, error) {
	n := *p
	n.validate = p.defaultEnabled
	if req.DNSSECValidation != nil {
		n.validate = *req.DNSSECValidation
	}
	group, conditional, err := p.set.Route(req.Question().Name, req.UpstreamGroup)
	if err != nil {
		return nil, err
	}
	n.group, n.conditional = group, conditional
	// Recursive groups validate under their own captured recursion settings.
	// The forwarded-answer validator must not change their CD behavior or trust.
	if n.Recursive() {
		n.validate = false
	}
	b := append([]byte(nil), p.set.identity[:]...)
	b = append(b, group...)
	b = append(b, 0)
	if n.validate {
		b = append(b, 1)
		b = append(b, p.active[:]...)
	} else {
		b = append(b, 0)
	}
	if req.Msg != nil && req.Msg.CheckingDisabled {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	n.scope.ID = sha256.Sum256(b)
	return &n, nil
}

func (p *QueryPlan) Scope() pipeline.CacheScope   { return p.scope }
func (p *QueryPlan) Validator() *dnssec.Validator { return p.validator }
func (p *QueryPlan) Group() string                { return p.group }
func (p *QueryPlan) ForCache() *QueryPlan         { n := *p; n.raw = true; return &n }
func (p *QueryPlan) RawResponse() bool            { return p.raw }
func (p *QueryPlan) Validates() bool              { return p.validate }
func (p *QueryPlan) Cacheable() bool              { return !p.conditional }

// Recursive reports whether this route uses the native validating resolver.
func (p *QueryPlan) Recursive() bool {
	g := p.set.groups[p.group]
	return g != nil && len(g.servers) == 1 && g.servers[0].proto == RecursiveServer
}
func (p *QueryPlan) Current() bool {
	return (p.current == nil || p.current()) && (p.validator == nil || p.active == p.validator.ActiveContextID())
}
func (p *QueryPlan) ECSSubnet(m *dns.Msg, _ netip.Addr) netip.Prefix {
	return p.set.ECSSubnet(m, p.client)
}

func (p *QueryPlan) Exchange(ctx context.Context, m *dns.Msg) (*Result, error) {
	q := p.set.PrepareQuery(m, p.client)
	q.AuthenticatedData = false
	if p.Recursive() {
		// Retain proofs for the raw cache; finalization strips them for non-DO clients.
		q.IsEdns0().SetDo()
	}
	if p.validate && !m.CheckingDisabled {
		q.CheckingDisabled = true
		q.IsEdns0().SetDo()
	}
	return p.set.Exchange(ctx, p.group, q)
}

// Lookup fetches full proofs on the captured route without client ECS or
// recursion through policy/cache/opcode handlers.
func (p *QueryPlan) Lookup(ctx context.Context, name string, typ uint16) (*dns.Msg, error) {
	q := new(dns.Msg)
	q.SetQuestion(name, typ)
	q.RecursionDesired = true
	q.CheckingDisabled = true
	q.SetEdns0(EDNSBufferSize, true)
	q.Id = nonZeroID()
	r, err := p.set.Exchange(ctx, p.group, q)
	if err != nil {
		return nil, err
	}
	return r.Msg, nil
}

// ScopeBytes is the key/persistence encoding of a scope.
func ScopeBytes(s pipeline.CacheScope) []byte {
	b := make([]byte, 40)
	copy(b, s.ID[:])
	binary.BigEndian.PutUint64(b[32:], s.Epoch)
	return b
}
