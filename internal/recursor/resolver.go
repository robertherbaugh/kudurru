package recursor

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/dnssec"
)

// Security is the DNSSEC state of an answer (RFC 4033 §5). The values
// match querylog.DNSSECState.
type Security uint8

// Security states.
const (
	// SecUnknown: validation is off, or the answer was not validated.
	SecUnknown Security = iota
	// SecSecure: every RRset is signed along a chain of trust from the
	// root trust anchors, and every denial is proven.
	SecSecure
	// SecInsecure: the answer comes from a zone the chain of trust proves
	// unsigned (a delegation without DS, or only unsupported algorithms).
	SecInsecure
	// SecBogus: signatures or proofs are missing or do not verify.
	SecBogus
	// SecIndeterminate is reserved (RFC 4033 §5); this resolver reports
	// bogus instead.
	SecIndeterminate
)

func (s Security) String() string {
	switch s {
	case SecSecure:
		return "secure"
	case SecInsecure:
		return "insecure"
	case SecBogus:
		return "bogus"
	case SecIndeterminate:
		return "indeterminate"
	}
	return "unknown"
}

// rank orders states from worst to best; an answer is as good as its
// worst part.
func (s Security) rank() int {
	switch s {
	case SecBogus:
		return 0
	case SecIndeterminate:
		return 1
	case SecInsecure:
		return 2
	case SecUnknown:
		return 3
	}
	return 4
}

// RRset identifies data whose authentication was explicitly excepted.
// It contains no wire proof and is produced only by native validation.
type RRset struct {
	Name string
	Type uint16
}

// Validation carries native state and the exact RRsets allowed by an NTA.
// Raw expired signatures for these sets are data, not authenticated deadlines.
type Validation struct {
	State    Security
	Excepted []RRset
}

// Excepts reports native operator-authorized provenance, never wire AD.
func (v Validation) Excepts(sig *dns.RRSIG) bool {
	if v.State != SecInsecure {
		return false
	}
	for _, set := range v.Excepted {
		if equalName(set.Name, sig.Hdr.Name) && set.Type == sig.TypeCovered {
			return true
		}
	}
	return false
}

// IPv6Mode decides whether name servers are asked over IPv6.
type IPv6Mode string

// IPv6 modes.
const (
	// IPv6Auto uses IPv6 when the host has a route to the IPv6 root
	// servers (checked when the settings are applied).
	IPv6Auto IPv6Mode = "auto"
	IPv6On   IPv6Mode = "on"
	IPv6Off  IPv6Mode = "off"
)

// Config is the resolver configuration (settings upstreams.recursion).
type Config struct {
	// DNSSEC turns DNSSEC validation on.
	DNSSEC bool
	// QNAMEMinimization sends each server only the labels it needs
	// (RFC 9156).
	QNAMEMinimization bool
	// CaseRandomization randomizes the letter case of query names (0x20).
	CaseRandomization bool
	IPv6              IPv6Mode
	// TrustAnchors replace the built-in root DS records; empty keeps them.
	TrustAnchors []string
	// NegativeTrustAnchors are independent of forwarded validation exceptions.
	NegativeTrustAnchors []dnssec.NTA
	// RootHints replace the built-in root server addresses; empty keeps
	// them.
	RootHints []string
	// CacheSize bounds the delegation, address and key cache (entries);
	// it is read once, by New.
	CacheSize int
}

// DefaultConfig is what an empty upstreams.recursion section means.
func DefaultConfig() Config {
	return Config{DNSSEC: true, QNAMEMinimization: true, CaseRandomization: true, IPv6: IPv6Auto, CacheSize: 50000}
}

// Validate checks the configuration.
func (c Config) Validate() error { return c.validate(time.Now()) }

func (c Config) validate(now time.Time) error {
	if _, err := dnssec.NewExceptions(c.NegativeTrustAnchors, now); err != nil {
		return err
	}
	switch c.IPv6 {
	case "", IPv6Auto, IPv6On, IPv6Off:
	default:
		return fmt.Errorf("recursor: unknown ipv6 mode %q (auto, on or off)", c.IPv6)
	}
	if len(c.TrustAnchors) > 0 {
		if _, err := ParseTrustAnchors(c.TrustAnchors); err != nil {
			return err
		}
	}
	if _, err := ParseRootHints(c.RootHints); err != nil {
		return err
	}
	if c.CacheSize < 0 {
		return errors.New("recursor: negative cache size")
	}
	return nil
}

// Options are the process-level dependencies.
type Options struct {
	Logger  *slog.Logger
	Metrics *Metrics
	// Now is the clock for caches and signature validity; nil means
	// time.Now.
	Now func() time.Time
	// Exchanger sends queries to authoritative servers; nil means plain
	// UDP and TCP sockets.
	Exchanger Exchanger
	// AllowLoopback lets the resolver ask servers on loopback addresses,
	// which it otherwise never does (tests).
	AllowLoopback bool
}

// Limits per client query.
const (
	maxQueries      = 120 // outgoing queries
	maxDepth        = 20  // nested resolutions (server addresses, keys)
	maxSteps        = 48  // referrals and minimization steps per name
	maxCNAME        = 12  // CNAME and DNAME hops
	maxServersAsk   = 8   // servers tried for one question
	maxGlueless     = 3   // name servers without glue resolved per question
	maxSigTries     = 8   // signature checks per RRset (KeyTrap)
	maxSigBudget    = 96  // signature checks per client query
	maxNSEC3Hashes  = 64  // NSEC3 name hashes per client query
	maxNSEC3Iter    = 150 // more iterations make a zone insecure (RFC 9276)
	maxMinimizeOne  = 4   // RFC 9156 MINIMISE_ONE_LAB
	maxMinimizeStep = 10  // RFC 9156 MAX_MINIMISE_COUNT
	maxCutProbes    = 8   // DS probes looking for an insecure delegation
)

// settings is one applied configuration.
type settings struct {
	cfg        Config
	exceptions *dnssec.Exceptions
	anchors    []*dns.DS
	hints      *zoneCut
	v6         bool
	// internalOK lets name servers have private, link-local and shared
	// addresses: only with operator-supplied root hints, i.e. a private
	// DNS tree.
	internalOK bool
	// gen changes with the trust anchors, root hints or validation, and
	// is part of every cache key (key).
	gen uint64
}

// Resolver is a validating recursive resolver. It is safe for concurrent
// use and keeps its caches across Reconfigure calls that keep the trust
// anchors and root hints.
type Resolver struct {
	set           atomic.Pointer[settings]
	ex            Exchanger
	cache         *lru
	infra         *infra
	metrics       *Metrics
	logger        *slog.Logger
	now           func() time.Time
	allowLoopback bool
	primeMu       sync.Mutex
	lastAnswer    atomic.Int64 // unix nanoseconds of the last answer from any name server
}

// reachableWindow is how recent an answer from a name server must be for
// Reachable.
const reachableWindow = 30 * time.Second

// Reachable reports whether some authoritative server answered within the
// last reachableWindow: the resolver can reach the internet, so a slow
// lookup is about the name, not about the network.
func (r *Resolver) Reachable() bool {
	t := r.lastAnswer.Load()
	return t != 0 && r.now().Sub(time.Unix(0, t)) < reachableWindow
}

// New builds a resolver. Nothing is sent until the first query.
func New(cfg Config, opts Options) (*Resolver, error) {
	r := &Resolver{
		ex:            opts.Exchanger,
		metrics:       opts.Metrics,
		logger:        opts.Logger,
		now:           opts.Now,
		allowLoopback: opts.AllowLoopback,
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.ex == nil {
		r.ex = netExchanger{cookies: newCookieJar(r.now)}
	}
	r.infra = newInfra(r.now)
	size := cfg.CacheSize
	if size <= 0 {
		size = DefaultConfig().CacheSize
	}
	r.cache = newLRU(size) // fixed for the resolver's life
	if err := r.Reconfigure(cfg); err != nil {
		return nil, err
	}
	return r, nil
}

// Reconfigure applies cfg. Changing the trust anchors, root hints,
// exceptions or validation drops the cached delegations and keys.
func (r *Resolver) Reconfigure(cfg Config) error {
	if err := cfg.validate(r.now()); err != nil {
		return err
	}
	if cfg.IPv6 == "" {
		cfg.IPv6 = IPv6Auto
	}
	if cfg.CacheSize == 0 {
		cfg.CacheSize = DefaultConfig().CacheSize
	}
	cfg.TrustAnchors = slices.Clone(cfg.TrustAnchors)
	cfg.RootHints = slices.Clone(cfg.RootHints)
	cfg.NegativeTrustAnchors = slices.Clone(cfg.NegativeTrustAnchors)
	exceptions, err := dnssec.NewExceptions(cfg.NegativeTrustAnchors, r.now())
	if err != nil {
		return err
	}
	anchors := DefaultTrustAnchors
	if len(cfg.TrustAnchors) > 0 {
		anchors = cfg.TrustAnchors
	}
	ds, err := ParseTrustAnchors(anchors)
	if err != nil {
		return err
	}
	hints, _ := ParseRootHints(cfg.RootHints) // validated above
	s := &settings{cfg: cfg, exceptions: exceptions, anchors: ds, hints: hintCut(hints), internalOK: len(hints) > 0}
	switch cfg.IPv6 {
	case IPv6On:
		s.v6 = true
	case IPv6Auto:
		s.v6 = haveIPv6Route()
	}
	prev := r.set.Load()
	s.gen = 1
	if prev != nil {
		s.gen = prev.gen
		if !slices.Equal(prev.cfg.NegativeTrustAnchors, cfg.NegativeTrustAnchors) || prev.cfg.DNSSEC != cfg.DNSSEC || !sameStrings(prev.cfg.TrustAnchors, cfg.TrustAnchors) || !sameStrings(prev.cfg.RootHints, cfg.RootHints) {
			// Queries still running with the old settings key their
			// writes by the old generation, so nothing they learn under
			// old anchors or hints is read again.
			s.gen++
			r.cache.flush()
		}
	}
	r.set.Store(s)
	return nil
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Config returns the configuration in effect.
func (r *Resolver) Config() Config {
	c := r.set.Load().cfg
	c.TrustAnchors = slices.Clone(c.TrustAnchors)
	c.RootHints = slices.Clone(c.RootHints)
	c.NegativeTrustAnchors = slices.Clone(c.NegativeTrustAnchors)
	return c
}

// TrustContext captures active recursive exceptions and the next absolute expiry.
// It performs no I/O and never consults forwarded validation configuration.
func (r *Resolver) TrustContext() ([32]byte, time.Time) {
	return r.set.Load().exceptions.Context(r.now())
}

// Flush drops every cached delegation, address and key.
func (r *Resolver) Flush() { r.cache.flush() }

// CacheLen is the number of cached delegations, addresses and keys.
func (r *Resolver) CacheLen() int { return r.cache.len() }

// Address implements dnsproxy's upstream.Upstream.
func (r *Resolver) Address() string { return "recursive" }

// Close implements dnsproxy's upstream.Upstream; the resolver holds no
// connections between queries.
func (r *Resolver) Close() error { return nil }

// Exchange implements dnsproxy's upstream.Upstream: it answers req by
// recursion. Resolution failures are SERVFAIL answers with an extended
// error; an error is returned only when ctx ends first.
func (r *Resolver) Exchange(ctx context.Context, req *dns.Msg) (*dns.Msg, error) {
	m, _, err := r.ExchangeValidated(ctx, req)
	return m, err
}

// ExchangeValidated is Exchange that also reports the DNSSEC state of the
// answer.
func (r *Resolver) ExchangeValidated(ctx context.Context, req *dns.Msg) (*dns.Msg, Security, error) {
	m, v, err := r.ExchangeWithValidation(ctx, req)
	return m, v.State, err
}

// ExchangeWithValidation retains exact exception provenance for the raw cache.
// Existing direct callers can continue using ExchangeValidated.
func (r *Resolver) ExchangeWithValidation(ctx context.Context, req *dns.Msg) (*dns.Msg, Validation, error) {
	if len(req.Question) != 1 {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeFormatError)
		return m, Validation{}, nil
	}
	question := req.Question[0]
	if question.Qclass != dns.ClassINET {
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeNotImplemented)
		m.RecursionAvailable = true
		return m, Validation{}, nil
	}
	s := r.set.Load()
	var a *answer
	var excepted []RRset
	if zone, ok := localZone(question.Name); ok {
		a = localAnswer(zone, question)
	} else {
		q := r.newQuery(ctx, s)
		switch question.Qtype {
		case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
			// Not RRsets of their own; returned unvalidated.
			q.validate = false
		}
		if !q.validate {
			q.st.exceptionUntil = time.Time{}
		}
		a = q.resolve(question.Name, question.Qtype)
		for set := range q.st.excepted {
			excepted = append(excepted, set)
		}
		if until := q.st.exceptionUntil; !until.IsZero() {
			if !r.now().Before(until) {
				a.sec, a.ede, a.why = SecBogus, dns.ExtendedErrorCodeSignatureExpired, "negative trust anchor expired during resolution"
			}
			// Direct resolver consumers also cannot cache an exception beyond
			// its original deadline, even with CD set. Never renew on reload.
			left := uint32(min(max(0, until.Sub(r.now())/time.Second), 1<<32-1)) //nolint:gosec // clamped to uint32 even if the clock moves backward
			for _, section := range [][]dns.RR{a.records, a.authority, a.proofs} {
				for _, rr := range section {
					rr.Header().Ttl = min(rr.Header().Ttl, left)
				}
			}
		}
		if a.fail != nil {
			if err := ctx.Err(); err != nil {
				return nil, Validation{}, err
			}
			// A socket deadline set from ctx can fire a moment before
			// ctx itself reports it: still the caller's deadline.
			if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) {
				return nil, Validation{}, context.DeadlineExceeded
			}
		}
	}
	if s.cfg.DNSSEC {
		r.metrics.validated(a.sec)
	}
	return reply(req, a), Validation{State: a.sec, Excepted: excepted}, nil
}

// clientState is shared by every nested lookup of one client query.
type clientState struct {
	budget         int
	exceptionUntil time.Time
	excepted       map[RRset]struct{}
	sigBudget      int
	hashBudget     int
	inKeys         map[string]bool
	tooDeep        bool // a lookup hit maxDepth
}

// query is one (possibly nested) lookup.
type query struct {
	r        *Resolver
	s        *settings
	ctx      context.Context
	st       *clientState
	depth    int
	validate bool
	active   string // exception activation captured before any native cache access
}

func (r *Resolver) newQuery(ctx context.Context, s *settings) *query {
	active, until := s.exceptions.Context(r.now())
	return &query{
		active: hex.EncodeToString(active[:]),
		r:      r, s: s, ctx: ctx, validate: s.cfg.DNSSEC,
		st: &clientState{exceptionUntil: until, budget: maxQueries, sigBudget: maxSigBudget, hashBudget: maxNSEC3Hashes, inKeys: map[string]bool{}},
	}
}

// nested returns a lookup one level deeper, sharing the budgets.
func (q *query) nested(validate bool) *query {
	return &query{r: q.r, s: q.s, ctx: q.ctx, st: q.st, depth: q.depth + 1, active: q.active, validate: validate && q.s.cfg.DNSSEC}
}

func (q *query) now() time.Time { return q.r.now() }

// key is the cache key of name under prefix for this query's settings.
func (q *query) key(prefix, name string) string {
	return prefix + strconv.FormatUint(q.s.gen, 36) + "|" + q.active + "|" + name
}

// limited reports whether this client query ran into one of its limits
// (or its deadline). Failures seen then say more about the query than
// about the names involved, so they are not cached for other queries.
func (q *query) limited() bool {
	st := q.st
	return st.budget <= 0 || st.sigBudget <= 0 || st.hashBudget <= 0 || st.tooDeep || q.ctx.Err() != nil
}

// answer is the outcome of a client query before it becomes a message.
type answer struct {
	rcode     int
	records   []dns.RR // answer section, with RRSIGs
	authority []dns.RR // negative answers: SOA, NSEC/NSEC3, RRSIGs
	proofs    []dns.RR // wildcard proofs, sent to DO clients
	sec       Security
	ede       uint16
	why       string
	fail      error // resolution failed: SERVFAIL
}

func (a *answer) merge(sec Security, ede uint16, why string) {
	if sec.rank() < a.sec.rank() {
		a.sec = sec
		if sec == SecBogus || ede != 0 {
			a.ede, a.why = ede, why
		}
	}
}

func (a *answer) failed(err error) *answer {
	a.fail = err
	a.sec = SecUnknown
	return a
}

// resolve answers name/qtype, following CNAME and DNAME chains across
// zones and validating every part.
func (q *query) resolve(qname string, qtype uint16) *answer {
	a := &answer{sec: SecSecure}
	if !q.validate {
		a.sec = SecUnknown
	}
	name := canon(qname)
	seen := map[string]bool{name: true}
	hops := 0
	for {
		if zone, ok := localZone(name); ok {
			// A CNAME or DNAME led into a locally served zone: answer it
			// here like a direct query, never on the internet.
			l := localAnswer(zone, dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET})
			a.records = append(a.records, l.records...)
			a.authority, a.rcode = l.authority, l.rcode
			a.merge(l.sec, 0, "")
			return a
		}
		res, err := q.iterate(name, qtype)
		if err != nil {
			return a.failed(err)
		}
		msg := res.msg
		cur := name
		for {
			if rrs := rrsetOf(msg.Answer, cur, qtype); len(rrs) > 0 {
				q.addRRset(a, rrs, msg, res.zone)
				a.rcode = dns.RcodeSuccess
				return a
			}
			if qtype == dns.TypeCNAME {
				break
			}
			// A DNAME above cur comes first: the CNAME beside it is
			// synthesized and unsigned (RFC 6672 §5.3.1).
			if dn := dnameFor(msg.Answer, cur); dn != nil && qtype != dns.TypeDNAME {
				q.addRRset(a, rrsetOf(msg.Answer, dn.Hdr.Name, dns.TypeDNAME), msg, res.zone)
				target, ok := substitute(cur, dn)
				if !ok {
					a.rcode = dns.RcodeYXDomain
					return a
				}
				a.records = append(a.records, &dns.CNAME{Hdr: hdr(cur, dns.TypeCNAME, dn.Hdr.Ttl), Target: target})
				cur = target
			} else if cn := rrsetOf(msg.Answer, cur, dns.TypeCNAME); len(cn) > 0 {
				q.addRRset(a, cn, msg, res.zone)
				cur = canon(cn[0].(*dns.CNAME).Target)
			} else {
				break
			}
			hops++
			if hops > maxCNAME || seen[cur] {
				return a.failed(errors.New("recursor: CNAME chain too long or looping"))
			}
			seen[cur] = true
		}
		if cur != name {
			// The chain left what this response holds; resolve the target.
			name = cur
			continue
		}
		// No data for name: NXDOMAIN or NODATA.
		a.rcode = msg.Rcode
		a.authority = negativeAuthority(msg.Ns)
		if q.validate {
			sec, ede, why := q.validateNegative(res, name, qtype)
			a.merge(sec, ede, why)
		}
		return a
	}
}

// addRRset appends rrset and its signatures to a and validates it.
func (q *query) addRRset(a *answer, rrset []dns.RR, msg *dns.Msg, zone string) {
	h := rrset[0].Header()
	sigs := sigsFor(msg.Answer, h.Name, h.Rrtype)
	a.records = append(a.records, rrset...)
	for _, s := range sigs {
		a.records = append(a.records, s)
	}
	if !q.validate {
		return
	}
	var v vres
	if h.Rrtype == dns.TypeDS {
		v = q.validateRRsetAbove(rrset, sigs, zone, h.Name) // the parent's, never the child's
	} else {
		v = q.validateRRset(rrset, sigs, zone)
	}
	if v.sec == SecSecure && v.wildcard != "" {
		proof, ok := q.proveWildcard(h.Name, v.wildcard, v.signer, msg.Ns, zone)
		if !ok {
			v = vres{sec: SecBogus, ede: dns.ExtendedErrorCodeNSECMissing, why: "wildcard answer for " + h.Name + " without proof that the name does not exist"}
		}
		a.proofs = append(a.proofs, proof...)
	}
	a.merge(v.sec, v.ede, v.why)
}

// negativeAuthority keeps the records a negative answer carries: SOA,
// NSEC, NSEC3 and their signatures.
func negativeAuthority(ns []dns.RR) []dns.RR {
	var out []dns.RR
	for _, rr := range ns {
		switch t := rr.Header().Rrtype; t {
		case dns.TypeSOA, dns.TypeNSEC, dns.TypeNSEC3:
			out = append(out, rr)
		case dns.TypeRRSIG:
			switch rr.(*dns.RRSIG).TypeCovered {
			case dns.TypeSOA, dns.TypeNSEC, dns.TypeNSEC3:
				out = append(out, rr)
			}
		}
	}
	return out
}

// rrsetOf returns the records of type t owned by name.
func rrsetOf(rrs []dns.RR, name string, t uint16) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		h := rr.Header()
		if h.Rrtype == t && equalName(h.Name, name) {
			out = append(out, rr)
		}
	}
	return out
}

// sigsFor returns the RRSIGs owned by name that cover type t.
func sigsFor(rrs []dns.RR, name string, t uint16) []*dns.RRSIG {
	var out []*dns.RRSIG
	for _, rr := range rrs {
		if s, ok := rr.(*dns.RRSIG); ok && s.TypeCovered == t && equalName(s.Hdr.Name, name) {
			out = append(out, s)
		}
	}
	return out
}

// dnameFor returns a DNAME owned by a proper ancestor of name.
func dnameFor(rrs []dns.RR, name string) *dns.DNAME {
	for _, rr := range rrs {
		if d, ok := rr.(*dns.DNAME); ok && isBelow(d.Hdr.Name, name) {
			return d
		}
	}
	return nil
}

// substitute applies a DNAME to name (RFC 6672 §2.2).
func substitute(name string, d *dns.DNAME) (string, bool) {
	prefix := name[:len(name)-len(d.Hdr.Name)]
	target := canon(prefix + canon(d.Target))
	if d.Target == "." {
		target = canon(prefix)
	}
	if _, ok := dns.IsDomainName(target); !ok || len(target) > 254 {
		return "", false
	}
	return target, true
}

// response is an authoritative answer for exactly the name and type asked.
type response struct {
	msg  *dns.Msg // scrubbed to the zone's bailiwick
	zone string   // the zone cut whose servers answered
}

// kind classifies a response from a zone's servers.
type kind uint8

const (
	kindLame kind = iota
	kindAnswer
	kindNoData
	kindNXDomain
	kindReferral
)

var (
	errSteps   = errors.New("recursor: too many referrals")
	errNoAuth  = errors.New("recursor: no name server of the zone answered")
	errLame    = errors.New("recursor: lame or unusable answer")
	errNoAddrs = errors.New("recursor: no usable name server address")
)

// iterate walks from the closest known delegation to the servers of the
// zone that holds name/qtype and returns their answer. A DS question is
// answered by the parent zone, so its walk stops above name.
func (q *query) iterate(name string, qtype uint16) (*response, error) {
	if q.depth > maxDepth {
		q.st.tooDeep = true
		return nil, errDepth
	}
	name = canon(name)
	start := name
	if qtype == dns.TypeDS && name != "." {
		start = parentName(name)
	}
	cut, err := q.closestCut(start)
	if err != nil {
		return nil, err
	}
	known := cut.name // the deepest name known to be in cut's zone
	minimize := q.s.cfg.QNAMEMinimization
	steps := 0
	for range maxSteps {
		sendName, sendType := name, qtype
		if minimize && labels(known) < labels(name) {
			if n := nextMinimized(known, name, steps); n != name {
				sendName, sendType = n, dns.TypeA
			}
			steps++
		}
		full := sendName == name && sendType == qtype
		resp, k, child, err := q.ask(cut, sendName, sendType)
		if err != nil {
			return nil, err
		}
		switch k {
		case kindReferral:
			if qtype == dns.TypeDS && child == name {
				// The parent delegated instead of answering DS itself:
				// the referral's DS evidence is the answer.
				return dsAnswerFromReferral(resp, cut.name, name), nil
			}
			cut = q.learnCut(resp, cut, child)
			known = child
		case kindAnswer, kindNoData:
			if full {
				return &response{msg: resp, zone: cut.name}, nil
			}
			if len(rrsetOf(resp.Answer, sendName, dns.TypeCNAME)) > 0 || dnameFor(resp.Answer, sendName) != nil {
				minimize = false // an alias has no children: ask for the name
				continue
			}
			known = sendName
		case kindNXDomain:
			if full {
				return &response{msg: resp, zone: cut.name}, nil
			}
			// Relaxed minimization: some servers deny empty non-terminals;
			// ask once more with the full name.
			minimize = false
		}
	}
	return nil, errSteps
}

// nextMinimized is the next name to ask for under RFC 9156 §2.3: one more
// label for the first steps, then bigger steps so a long name takes at
// most maxMinimizeStep queries.
func nextMinimized(known, name string, steps int) string {
	if steps >= maxMinimizeStep {
		return name
	}
	add := 1
	if steps >= maxMinimizeOne {
		remaining := labels(name) - labels(known)
		left := maxMinimizeStep - steps
		add = max(1, (remaining+left-1)/left)
	}
	return suffixLabels(name, labels(known)+add)
}

// dsAnswerFromReferral turns a referral's DS evidence into a DS answer.
func dsAnswerFromReferral(resp *dns.Msg, zone, name string) *response {
	m := new(dns.Msg)
	m.Rcode = dns.RcodeSuccess
	m.Authoritative = true
	for _, rr := range resp.Ns {
		switch h := rr.Header(); {
		case h.Rrtype == dns.TypeDS && equalName(h.Name, name):
			m.Answer = append(m.Answer, rr)
		case h.Rrtype == dns.TypeRRSIG && rr.(*dns.RRSIG).TypeCovered == dns.TypeDS && equalName(h.Name, name):
			m.Answer = append(m.Answer, rr)
		case h.Rrtype == dns.TypeNSEC || h.Rrtype == dns.TypeNSEC3 || h.Rrtype == dns.TypeRRSIG || h.Rrtype == dns.TypeSOA:
			m.Ns = append(m.Ns, rr)
		}
	}
	return &response{msg: m, zone: zone}
}

// closestCut returns the deepest cached delegation at or above name.
func (q *query) closestCut(name string) (*zoneCut, error) {
	now := q.now()
	for n := canon(name); n != "."; n = parentName(n) {
		if v, ok := q.r.cache.get(q.key(kCut, n), now); ok {
			return v.(*zoneCut), nil
		}
	}
	return q.rootCut()
}

// rootCut returns the root delegation, priming it from the hints when it
// is not cached (RFC 9609). A failed priming falls back to the hints for
// failTTL.
func (q *query) rootCut() (*zoneCut, error) {
	r := q.r
	if v, ok := r.cache.get(q.key(kCut, "."), q.now()); ok {
		return v.(*zoneCut), nil
	}
	hints := q.s.hints
	// One query primes; the others meanwhile start from the hints instead
	// of waiting on its network round trip.
	if !r.primeMu.TryLock() {
		return hints, nil
	}
	defer r.primeMu.Unlock()
	if v, ok := r.cache.get(q.key(kCut, "."), q.now()); ok {
		return v.(*zoneCut), nil
	}
	resp, k, _, err := q.ask(hints, ".", dns.TypeNS)
	if err == nil && k == kindAnswer {
		ns := rrsetOf(resp.Answer, ".", dns.TypeNS)
		cut := &zoneCut{name: ".", glue: map[string][]netip.AddrPort{}}
		for _, rr := range ns {
			cut.ns = appendUnique(cut.ns, canon(rr.(*dns.NS).Ns))
		}
		addGlue(cut, resp.Extra, ".", hints.port())
		if len(cut.glue) >= 2 {
			r.cache.set(q.key(kCut, "."), cut, ttlOf(ns...), q.now())
			return cut, nil
		}
	}
	if errors.Is(err, errBudget) || q.ctx.Err() != nil {
		return hints, nil
	}
	r.logger.Debug("recursor: root priming failed; using the root hints", "err", err)
	r.cache.set(q.key(kCut, "."), hints, failTTL, q.now())
	return hints, nil
}

// port is the port glue addresses get: the hints' own (tests), else 53.
func (c *zoneCut) port() uint16 {
	for _, g := range c.glue {
		for _, a := range g {
			return a.Port()
		}
	}
	return 53
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// addGlue records the A and AAAA records in extra for the cut's name
// servers that lie inside bailiwick (the delegating zone).
func addGlue(cut *zoneCut, extra []dns.RR, bailiwick string, port uint16) {
	want := map[string]bool{}
	for _, n := range cut.ns {
		want[n] = true
	}
	for _, rr := range extra {
		h := rr.Header()
		name := canon(h.Name)
		if !want[name] || !isSubOrEqual(bailiwick, name) {
			continue
		}
		var ip netip.Addr
		switch v := rr.(type) {
		case *dns.A:
			ip, _ = netip.AddrFromSlice(v.A.To4())
		case *dns.AAAA:
			ip, _ = netip.AddrFromSlice(v.AAAA.To16())
		default:
			continue
		}
		if ip.IsValid() {
			cut.glue[name] = append(cut.glue[name], netip.AddrPortFrom(ip, port))
		}
	}
}

// learnCut caches the delegation to child from a referral by parent's
// servers, and keeps the referral's DS evidence for validation.
func (q *query) learnCut(resp *dns.Msg, parent *zoneCut, child string) *zoneCut {
	ns := rrsetOf(resp.Ns, child, dns.TypeNS)
	cut := &zoneCut{name: child, glue: map[string][]netip.AddrPort{}}
	for _, rr := range ns {
		cut.ns = appendUnique(cut.ns, canon(rr.(*dns.NS).Ns))
	}
	addGlue(cut, resp.Extra, parent.name, parent.port())
	now := q.now()
	q.r.cache.set(q.key(kCut, child), cut, ttlOf(ns...), now)
	if q.s.cfg.DNSSEC {
		var ev []dns.RR
		for _, rr := range resp.Ns {
			switch rr.Header().Rrtype {
			case dns.TypeDS, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeRRSIG:
				ev = append(ev, rr)
			}
		}
		if len(ev) > 0 {
			q.r.cache.set(q.key(kRefD, child), &referralDS{zone: parent.name, records: ev}, ttlOf(ns...), now)
		}
	}
	return cut
}

// ask puts name/qtype to the servers of cut until one gives a usable
// answer, which it returns scrubbed and classified (with the child zone of
// a referral).
func (q *query) ask(cut *zoneCut, name string, qtype uint16) (*dns.Msg, kind, string, error) {
	tried := map[netip.AddrPort]bool{}
	lastErr := errNoAddrs
	sent := 0
	for pass := range 2 {
		for _, a := range q.r.infra.order(q.cutAddrs(cut, pass == 1)) {
			if tried[a] {
				continue
			}
			tried[a] = true
			if sent >= maxServersAsk {
				return nil, kindLame, "", lastErr
			}
			if err := q.ctx.Err(); err != nil {
				return nil, kindLame, "", err
			}
			sent++
			resp, err := q.send(a, name, qtype)
			if err != nil {
				lastErr = err
				if errors.Is(err, errBudget) {
					return nil, kindLame, "", err
				}
				continue
			}
			if resp.Rcode != dns.RcodeSuccess && resp.Rcode != dns.RcodeNameError {
				lastErr = fmt.Errorf("%w: %s answered %s", errNoAuth, a.Addr(), dns.RcodeToString[resp.Rcode])
				q.r.infra.fail(a.Addr(), false)
				continue
			}
			scrub(resp, cut.name)
			k, child := classify(resp, cut.name, name, qtype)
			if k == kindLame {
				lastErr = fmt.Errorf("%w from %s for %s", errLame, a.Addr(), cut.name)
				q.r.infra.fail(a.Addr(), false)
				continue
			}
			return resp, k, child, nil
		}
	}
	if errors.Is(lastErr, errNoAddrs) || isTimeout(lastErr) {
		return nil, kindLame, "", fmt.Errorf("%w %s: %w", errNoAuth, cut.name, lastErr)
	}
	return nil, kindLame, "", lastErr
}

// cutAddrs lists the server addresses of cut: glue and cached addresses,
// and with resolve also the addresses of a few name servers that had
// none (name servers inside the zone itself cannot be resolved without
// glue and are skipped).
func (q *query) cutAddrs(cut *zoneCut, resolve bool) []netip.AddrPort {
	var out []netip.AddrPort
	var missing []string
	now := q.now()
	port := cut.port()
	for _, ns := range cut.ns {
		if g := cut.glue[ns]; len(g) > 0 {
			out = append(out, g...)
			continue
		}
		if v, ok := q.r.cache.get(q.key(kAddr, ns), now); ok {
			for _, ip := range v.(addrSet) {
				out = append(out, netip.AddrPortFrom(ip, port))
			}
			continue
		}
		missing = append(missing, ns)
	}
	if resolve {
		rand.Shuffle(len(missing), func(i, j int) { missing[i], missing[j] = missing[j], missing[i] }) //nolint:gosec // load spreading
		n := 0
		for _, ns := range missing {
			if n >= maxGlueless || isSubOrEqual(cut.name, ns) {
				continue
			}
			n++
			for _, ip := range q.resolveAddrs(ns) {
				out = append(out, netip.AddrPortFrom(ip, port))
			}
		}
	}
	return q.usable(out)
}

// usable drops addresses the resolver must not or cannot ask.
//
// Name-server addresses come from untrusted glue and lookups: with the
// public root, a zone could otherwise delegate to 169.254.169.254, 10.x
// or fd00:: hosts and have the resolver probe the local network on its
// behalf. Such addresses are asked only in a private tree (custom root
// hints).
func (q *query) usable(in []netip.AddrPort) []netip.AddrPort {
	out := in[:0]
	for _, a := range in {
		ip := a.Addr().Unmap()
		switch {
		case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast():
			continue
		case ip.IsLoopback() && !q.r.allowLoopback:
			continue
		case !ip.IsLoopback() && internalAddr(ip) && !q.s.internalOK:
			continue
		case ip.Is6() && !q.s.v6:
			continue
		}
		out = append(out, netip.AddrPortFrom(ip, a.Port()))
	}
	return out
}

// Address ranges that are not on the public internet, besides loopback.
var (
	thisNetwork = netip.MustParsePrefix("0.0.0.0/8")     // RFC 1122
	sharedSpace = netip.MustParsePrefix("100.64.0.0/10") // RFC 6598 (CGNAT)
	ietfV4      = netip.MustParsePrefix("192.0.0.0/24")  // RFC 6890
	testNet1    = netip.MustParsePrefix("192.0.2.0/24")  // RFC 5737
	testNet2    = netip.MustParsePrefix("198.51.100.0/24")
	testNet3    = netip.MustParsePrefix("203.0.113.0/24")
	benchmark   = netip.MustParsePrefix("198.18.0.0/15")  // RFC 2544
	reservedV4  = netip.MustParsePrefix("240.0.0.0/4")    // RFC 1112
	siteLocal   = netip.MustParsePrefix("fec0::/10")      // RFC 3879
	discardV6   = netip.MustParsePrefix("100::/64")       // RFC 6666
	docV6       = netip.MustParsePrefix("2001:db8::/32")  // RFC 3849
	nat64Local  = netip.MustParsePrefix("64:ff9b:1::/48") // RFC 8215
)

// internalAddr reports whether ip belongs to a private, link-local,
// shared, documentation or otherwise non-public range.
func internalAddr(ip netip.Addr) bool {
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range []netip.Prefix{thisNetwork, sharedSpace, ietfV4, testNet1, testNet2, testNet3, benchmark, reservedV4, siteLocal, discardV6, docV6, nat64Local} {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// resolveAddrs looks up a name server's addresses (not validated: they
// only choose where to ask, and answers are validated on their own).
func (q *query) resolveAddrs(host string) addrSet {
	now := q.now()
	if v, ok := q.r.cache.get(q.key(kAddr, host), now); ok {
		return v.(addrSet)
	}
	sub := q.nested(false)
	if sub.depth > maxDepth {
		q.st.tooDeep = true
		return nil
	}
	var out addrSet
	var ttl time.Duration
	types := []uint16{dns.TypeA}
	if q.s.v6 {
		types = append(types, dns.TypeAAAA)
	}
	failed := false
	for _, t := range types {
		a := sub.resolve(host, t)
		if a.fail != nil {
			failed = true
			continue
		}
		for _, rr := range a.records {
			var ip netip.Addr
			switch v := rr.(type) {
			case *dns.A:
				ip, _ = netip.AddrFromSlice(v.A.To4())
			case *dns.AAAA:
				ip, _ = netip.AddrFromSlice(v.AAAA.To16())
			default:
				continue
			}
			if ip.IsValid() {
				out = append(out, ip)
				if d := time.Duration(rr.Header().Ttl) * time.Second; ttl == 0 || d < ttl {
					ttl = d
				}
			}
		}
	}
	if len(out) == 0 {
		if !failed || !q.limited() {
			q.r.cache.set(q.key(kAddr, host), addrSet(nil), failTTL, now)
		}
		return nil
	}
	q.r.cache.set(q.key(kAddr, host), out, ttl, now)
	return out
}

// scrub drops what a zone's servers may not speak for: records outside the
// zone, other classes, and anything but name-server addresses in the
// additional section. Owner names are lower-cased (0x20 echoes).
func scrub(m *dns.Msg, zone string) {
	keep := func(rrs []dns.RR, ok func(dns.RR) bool) []dns.RR {
		out := rrs[:0]
		for _, rr := range rrs {
			h := rr.Header()
			if h.Rrtype == dns.TypeOPT || h.Class != dns.ClassINET || !isSubOrEqual(zone, h.Name) || !ok(rr) {
				continue
			}
			h.Name = canon(h.Name)
			out = append(out, rr)
		}
		return out
	}
	m.Answer = keep(m.Answer, func(dns.RR) bool { return true })
	m.Ns = keep(m.Ns, func(rr dns.RR) bool {
		switch rr.Header().Rrtype {
		case dns.TypeNS, dns.TypeSOA, dns.TypeDS, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeRRSIG:
			return true
		}
		return false
	})
	m.Extra = keep(m.Extra, func(rr dns.RR) bool {
		t := rr.Header().Rrtype
		return t == dns.TypeA || t == dns.TypeAAAA
	})
}

// classify sorts a scrubbed response to name/qtype from zone's servers.
func classify(m *dns.Msg, zone, name string, qtype uint16) (kind, string) {
	hasSOA := len(rrsetOfType(m.Ns, dns.TypeSOA)) > 0
	if m.Rcode == dns.RcodeNameError {
		if m.Authoritative || hasSOA {
			return kindNXDomain, ""
		}
		return kindLame, ""
	}
	for _, rr := range m.Answer {
		h := rr.Header()
		if equalName(h.Name, name) && (h.Rrtype == qtype || h.Rrtype == dns.TypeCNAME) ||
			h.Rrtype == dns.TypeDNAME && isBelow(h.Name, name) {
			if m.Authoritative {
				return kindAnswer, ""
			}
			return kindLame, ""
		}
	}
	child := ""
	for _, rr := range m.Ns {
		h := rr.Header()
		if h.Rrtype == dns.TypeNS && isBelow(zone, h.Name) && isSubOrEqual(h.Name, name) && (child == "" || labels(h.Name) > labels(child)) {
			child = h.Name
		}
	}
	if child != "" && !hasSOA {
		return kindReferral, child
	}
	if m.Authoritative {
		return kindNoData, ""
	}
	return kindLame, ""
}

func rrsetOfType(rrs []dns.RR, t uint16) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		if rr.Header().Rrtype == t {
			out = append(out, rr)
		}
	}
	return out
}

// reply builds the client message for a.
func reply(req *dns.Msg, a *answer) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	m.CheckingDisabled = req.CheckingDisabled
	opt := req.IsEdns0()
	do := opt != nil && opt.Do()
	qtype := req.Question[0].Qtype
	if a.fail != nil || a.sec == SecBogus && !req.CheckingDisabled {
		m.Rcode = dns.RcodeServerFailure
		code, text := a.ede, a.why
		if a.fail != nil {
			code, text = failureEDE(a.fail)
		}
		addEDE(m, code, text)
		return m
	}
	m.Rcode = a.rcode
	m.Answer = forClient(a.records, do, qtype)
	m.Ns = forClient(a.authority, do, qtype)
	if do {
		m.Ns = append(m.Ns, a.proofs...)
	}
	for _, rr := range m.Ns {
		if soa, ok := rr.(*dns.SOA); ok {
			soa.Hdr.Ttl = min(soa.Hdr.Ttl, soa.Minttl) // RFC 2308 §5
		}
	}
	m.AuthenticatedData = a.sec == SecSecure && (do || req.AuthenticatedData)
	if a.sec == SecInsecure && a.ede != 0 {
		addEDE(m, a.ede, a.why)
	}
	return m
}

// forClient copies rrs, leaving out DNSSEC records for clients that did
// not set DO, unless the client asked for that type.
func forClient(rrs []dns.RR, do bool, qtype uint16) []dns.RR {
	out := make([]dns.RR, 0, len(rrs))
	for _, rr := range rrs {
		switch t := rr.Header().Rrtype; t {
		case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
			if !do && t != qtype {
				continue
			}
		}
		out = append(out, dns.Copy(rr))
	}
	return out
}

// failureEDE maps a resolution failure to an extended error.
func failureEDE(err error) (uint16, string) {
	switch {
	case errors.Is(err, errBudget), errors.Is(err, errDepth), errors.Is(err, errSteps):
		return dns.ExtendedErrorCodeOther, "recursion limit reached"
	case errors.Is(err, errNoAuth), errors.Is(err, errNoAddrs), isTimeout(err):
		return dns.ExtendedErrorCodeNoReachableAuthority, "no name server answered"
	}
	return dns.ExtendedErrorCodeNetworkError, err.Error()
}

func addEDE(m *dns.Msg, code uint16, text string) {
	m.SetEdns0(ednsSize, false)
	opt := m.IsEdns0()
	opt.Option = append(opt.Option, &dns.EDNS0_EDE{InfoCode: code, ExtraText: text})
}
