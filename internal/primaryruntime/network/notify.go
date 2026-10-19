package network

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// NotifyCut retains credentials from ONE protected complete dependency cut.
// Current must check continuing membership/ancestor deadlines as well as writer
// and dependencies, using cheap memory predicates safe in CommandTx/final hooks.
// Release relinquishes resource ownership, never a held transaction pin.
type NotifyCut struct {
	Identity string
	Keys     *dnsserver.StaticKeyring
	Current  func() bool
	Release  func()
}

// CaptureNotify validates exact current published membership and writer before
// returning same-cut credentials. It must not substitute independently captured
// keys. Ownership transfers even when a cut accompanies an error.
type CaptureNotify func(context.Context, pr.PublicationFence, pr.Member) (NotifyCut, error)

// Resolver performs bounded resolution outside every admission pin.
type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

// NotifyOptions supplies root's capture and finite caps. Zero uses defaults;
// larger-than-default caps are rejected, never silently truncated.
type NotifyOptions struct {
	Capture                                         CaptureNotify
	Resolver                                        Resolver
	SourceAddr                                      netip.Addr
	MaxTargets, MaxNames, MaxAddresses, Parallelism int
}

// Sender binds private attempts to both current admission and actual sends.
// It keeps no successful-target ledger; durable acknowledgment belongs to core.
type Sender struct{ options NotifyOptions }

// NewNotify creates a bounded sender. Missing Capture leaves delivery disabled.
func NewNotify(o NotifyOptions) (*Sender, error) {
	for _, bound := range []struct {
		value *int
		max   int
	}{
		{&o.MaxTargets, 64}, {&o.MaxNames, 64}, {&o.MaxAddresses, 8}, {&o.Parallelism, 4},
	} {
		if *bound.value < 0 || *bound.value > bound.max {
			return nil, pr.ErrCapacity
		}
		if *bound.value == 0 {
			*bound.value = bound.max
		}
	}
	if o.SourceAddr.IsValid() && o.SourceAddr.Zone() != "" {
		return nil, pr.ErrInvalid
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	return &Sender{options: o}, nil
}

type attemptKey struct{}

const (
	attemptBound uint32 = iota
	attemptAdmitted
	attemptSending
	attemptSent
	attemptAck
	attemptClosed
)

type attempt struct {
	owner  *Sender
	ctx    context.Context
	cut    NotifyCut
	fence  pr.PublicationFence
	member pr.Member
	model  *zone.Zone
	policy notifyPolicy
	soa    *dns.SOA
	serial uint32
	names  []zone.NameServer
	key    dnsserver.TSIGIdentity
	state  atomic.Uint32
	intent atomic.Uint64
}

// Bind captures same-cut credentials and frozen policy without DNS/network I/O.
// The capability is issuer-private and used unchanged through delivery/ack.
func (s *Sender) Bind(ctx context.Context, m pr.Member, z *zone.Zone) (context.Context, func(), error) {
	if s == nil || s.options.Capture == nil || ctx == nil || ctx.Err() != nil || z == nil || m.Type != public.ZoneTypePrimary || m.Phase != zk.PhaseActive || !m.Published || m.Incarnation == 0 || m.Sequence == 0 || m.Version.CommitSequence == 0 || !zk.ValidDigest(m.Version.RootDigest) || len(m.Parents) > zk.MaxParentFences || z.Type() != public.ZoneTypePrimary || z.Name() != m.Zone.Apex || !z.AvailableAt(time.Now()) {
		return nil, nil, pr.ErrUnavailable
	}
	f, ok := pr.PublicationFenceFrom(ctx)
	if !ok || f.Term.Validate() != nil || f.Incarnation == 0 || f.Term.ClusterID != m.Zone.ClusterID || m.Version.Zone.ClusterID != m.Zone.ClusterID || m.Version.Zone.ObjectName != m.Zone.ObjectName || m.Version.Zone.Apex != m.Zone.Apex {
		return nil, nil, pr.ErrUnavailable
	}
	m.Parents = slices.Clone(m.Parents)
	cut, err := s.options.Capture(ctx, f, m)
	var once sync.Once
	release := func() {
		once.Do(func() {
			if cut.Release != nil {
				cut.Release()
			}
		})
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	if cut.Keys == nil || cut.Identity == "" || len(cut.Identity) > 512 || cut.Current == nil || !cut.Current() {
		release()
		return nil, nil, pr.ErrUnavailable
	}
	p, err := s.modelPolicy(z)
	if err != nil {
		release()
		return nil, nil, err
	}
	names := z.NameServers()
	if len(names) > s.options.MaxNames {
		release()
		return nil, nil, pr.ErrCapacity
	}
	for _, ns := range names {
		if len(ns.Addrs) > s.options.MaxAddresses {
			release()
			return nil, nil, pr.ErrCapacity
		}
	}
	soa := z.SOA()
	if soa == nil {
		release()
		return nil, nil, pr.ErrUnavailable
	}
	a := &attempt{owner: s, ctx: ctx, cut: cut, fence: f, member: m, model: z, policy: p, soa: soa, serial: z.Serial(), names: names}
	if len(p.keys) != 0 && p.mode != public.NotifyNone {
		var found bool
		a.key, found = cut.Keys.Identity(p.keys[0])
		if !found {
			release()
			return nil, nil, pr.ErrUnavailable
		}
	}
	return context.WithValue(ctx, attemptKey{}, a), func() { a.state.Store(attemptClosed); release() }, nil
}

func (s *Sender) bound(ctx context.Context, m pr.Member, z *zone.Zone) (*attempt, error) {
	if ctx == nil || s == nil {
		return nil, pr.ErrUnavailable
	}
	a, ok := ctx.Value(attemptKey{}).(*attempt)
	if !ok || a == nil || a.owner != s || a.model != z || !sameMember(a.member, m) {
		return nil, pr.ErrUnavailable
	}
	return a, a.current(ctx)
}

func (a *attempt) current(ctx context.Context) error {
	if a.state.Load() == attemptClosed || a.ctx.Err() != nil || ctx.Err() != nil || !a.cut.Current() || !a.model.AvailableAt(time.Now()) {
		return pr.ErrUnavailable
	}
	f, ok := pr.PublicationFenceFrom(ctx)
	if !ok || f != a.fence {
		return pr.ErrUnavailable
	}
	return nil
}

func sameMember(a, b pr.Member) bool {
	a.Published, b.Published = false, false
	return reflect.DeepEqual(a, b)
}

// Validator closes over this guard pin's immutable keys/identity. It requires
// this Sender's private attempt and uses only actual CommandTx reads. Success
// from Send is not durable acknowledgment and can never skip exact head CAS.
func (s *Sender) Validator(identity string, keys *dnsserver.StaticKeyring) sqliteadapter.BackgroundPolicy {
	return func(ctx context.Context, tx *sqlite.CommandTx, c sqliteadapter.BackgroundCheck) error {
		if s == nil || ctx == nil || tx == nil || keys == nil || identity == "" || c.Lifecycle == nil || c.Source == nil || c.Policy == nil || (c.Reason != "notify-delivery" && c.Reason != "notify-ack") {
			return pr.ErrUnavailable
		}
		a, ok := ctx.Value(attemptKey{}).(*attempt)
		if !ok || a == nil || a.owner != s || identity != a.cut.Identity || keys != a.cut.Keys {
			return pr.ErrUnavailable
		}
		if err := a.current(ctx); err != nil {
			return err
		}
		l := c.Lifecycle
		if !a.fence.Term.Same(tx.Authority()) || a.fence.Incarnation != tx.Incarnation() || l.Spec.Zone != a.member.Zone || l.Spec.Type != a.member.Type || l.Spec.Phase != a.member.Phase || l.Spec.Incarnation != a.member.Incarnation || l.Spec.Sequence != a.member.Sequence || !reflect.DeepEqual(l.Spec.Parents, a.member.Parents) {
			return pr.ErrConflict
		}
		policy := c.Source.Spec.Policy
		if c.Source.Spec.Fallback != nil {
			policy.Notify = public.NotifyNone
		}
		p, err := s.sourcePolicy(policy)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(p, a.policy) {
			return pr.ErrConflict
		}
		if a.key.Key != "" {
			id, found := keys.Identity(a.key.Key)
			if !found || id != a.key {
				return pr.ErrUnavailable
			}
		}
		var head zk.ZoneState
		if err := tx.Get(ctx, store.Key{Kind: zk.KindZoneState, Name: zk.HeadName(l.Spec.HeadZone())}, &head); err != nil {
			return err
		}
		v := a.member.Version
		if head.Validate() != nil || head.Spec.Lifecycle != zk.LifecycleActive || head.Spec.Zone != v.Zone || head.Spec.CommitSequence != v.CommitSequence || head.Spec.RootDigest != v.RootDigest || head.Spec.NotifySequence == 0 || head.Spec.NotifySerial != a.serial || head.Spec.Serial != a.serial {
			return pr.ErrConflict
		}
		from, to := attemptBound, attemptAdmitted
		if c.Reason == "notify-ack" {
			from, to = attemptSent, attemptAck
			if a.intent.Load() != head.Spec.NotifySequence {
				return pr.ErrConflict
			}
		}
		if !a.state.CompareAndSwap(from, to) {
			return pr.ErrUnavailable
		}
		if c.Reason == "notify-delivery" {
			a.intent.Store(head.Spec.NotifySequence)
		}
		return tx.RequireCommitCheck(func(finalCtx context.Context) error {
			if a.state.Load() != to {
				return pr.ErrUnavailable
			}
			return a.current(finalCtx)
		})
	}
}

type notifyPolicy struct {
	mode    public.NotifyMode
	targets []netip.AddrPort
	keys    []string
}

func (s *Sender) modelPolicy(z *zone.Zone) (notifyPolicy, error) {
	p := notifyPolicy{mode: z.NotifyMode(), targets: z.NotifyTargets(), keys: z.TransferKeys()}
	if len(p.targets) > s.options.MaxTargets || len(p.keys) > 64 {
		return p, pr.ErrCapacity
	}
	p.targets = normalizedTargets(p.targets)
	return p, nil
}

func (s *Sender) sourcePolicy(p public.ZonePolicy) (notifyPolicy, error) {
	out := notifyPolicy{mode: p.Notify}
	if out.mode == "" {
		out.mode = public.NotifyNone
	}
	switch out.mode {
	case public.NotifyNone, public.NotifySpecified, public.NotifyZoneNameServers, public.NotifyBoth:
	default:
		return out, pr.ErrInvalid
	}
	if len(p.NotifyTargets) > s.options.MaxTargets || len(p.TransferTSIGKeyRefs) > 64 {
		return out, pr.ErrCapacity
	}
	for _, target := range p.NotifyTargets {
		ap, err := zone.ParseNotifyTarget(target)
		if err != nil {
			return out, pr.ErrInvalid
		}
		out.targets = append(out.targets, ap)
	}
	out.targets = normalizedTargets(out.targets)
	for _, ref := range p.TransferTSIGKeyRefs {
		key := zone.KeyName(ref)
		if len(ref) > 255 || strings.TrimSpace(ref) == "" || key == "." {
			return out, pr.ErrInvalid
		}
		if _, valid := dns.IsDomainName(key); !valid {
			return out, pr.ErrInvalid
		}
		if !slices.Contains(out.keys, key) {
			out.keys = append(out.keys, key)
		}
	}
	return out, nil
}

func normalizedTargets(targets []netip.AddrPort) []netip.AddrPort {
	slices.SortFunc(targets, func(a, b netip.AddrPort) int { return a.Compare(b) })
	return slices.Compact(targets)
}
