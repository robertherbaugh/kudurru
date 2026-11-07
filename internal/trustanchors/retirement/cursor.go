package retirement

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// History fixes exact local comparison labels; these labels attest no history.
type History struct {
	ClusterID string
	HistoryID string
}

// Mark is a complete mechanical managed-owner version, not accepted evidence.
type Mark struct {
	DomainID           [32]byte
	EnvelopeGeneration uint64
	EnvelopeDigest     [32]byte
	PureGeneration     uint64
	PureDigest         [32]byte
}

// Frame includes the full projection identity, not just effective trust keys.
// Supplying a Frame cannot construct a read registration or serving permit.
type Frame struct {
	Mark    Mark
	OwnerID [32]byte
}

// Snapshot is an opaque mechanical registration. Only a future independently
// admitted private owner can bind it to real complete response authority.
type Snapshot struct {
	gate  *Gate
	frame Frame
}

func validHistory(h History) bool {
	if len(h.ClusterID) == 0 || len(h.ClusterID) > 256 || !utf8.ValidString(h.ClusterID) || strings.TrimSpace(h.ClusterID) != h.ClusterID || len(h.HistoryID) != 32 {
		return false
	}
	for _, r := range h.ClusterID {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, r := range h.HistoryID {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validMark(m Mark) bool {
	return m.DomainID != ([32]byte{}) && m.EnvelopeDigest != ([32]byte{}) && m.PureDigest != ([32]byte{}) && m.PureGeneration >= 1 && m.PureGeneration < math.MaxUint64-1 && m.EnvelopeGeneration == m.PureGeneration+1 && m.EnvelopeGeneration < math.MaxUint64
}

// All nodes are immutable after publication and never reused. The same pointer
// CAS arbitrates scope close, plan phase, installed owner, floor and binding.
// Queue accounting is independent; a scope closes BEFORE releasing its slot.
type gateState struct {
	closed    bool
	domain    [32]byte
	installed Mark
	floor     uint64
	current   *Snapshot
	writer    *writeLease
	plan      planState
}

type planKind uint8

const (
	changePlan planKind = iota + 1
	bindingPlan
	withdrawalPlan
)

type planToken struct{ tag byte }

type planState struct {
	token    *planToken
	kind     planKind
	target   Mark
	staged   bool
	applied  bool
	resolved bool
	invalid  bool
	effect   Effect
}

type planRef struct {
	lease *writeLease
	token *planToken
}

func (r planRef) load() (*gateState, error) {
	if r.lease == nil || r.token == nil {
		return nil, ErrScope
	}
	old := r.lease.gate.state.Load()
	if old.writer != r.lease || old.plan.token != r.token {
		return nil, ErrScope
	}
	return old, nil
}

// fail can poison ONLY this active scope. After the scope-close CAS or a
// successor writer, an escaped call has no ability to close/mutate the gate.
func (l *writeLease) fail(reason error) error {
	if l == nil {
		return ErrScope
	}
	for {
		old := l.gate.state.Load()
		if old.writer != l {
			return ErrScope
		}
		if old.closed {
			return reason
		}
		next := *old
		next.closed, next.current, next.plan.invalid = true, nil, true
		if next.plan.effect == NoEffect {
			next.plan.effect = UnknownEffect
		}
		if l.gate.state.CompareAndSwap(old, &next) {
			return reason
		}
	}
}

func (s *WriteScope) begin(kind planKind) (planRef, *gateState, error) {
	if s == nil || s.lease == nil {
		return planRef{}, nil, ErrScope
	}
	token := &planToken{tag: 1}
	for {
		old := s.lease.gate.state.Load()
		if old.writer != s.lease {
			return planRef{}, nil, ErrScope
		}
		if old.closed {
			return planRef{}, nil, ErrClosed
		}
		if old.plan.token != nil {
			return planRef{}, nil, s.lease.fail(ErrUsed)
		}
		if err := s.lease.life.check(); err != nil {
			return planRef{}, nil, err
		}
		next := *old
		next.plan = planState{token: token, kind: kind}
		if s.lease.gate.state.CompareAndSwap(old, &next) {
			return planRef{lease: s.lease, token: token}, &next, nil
		}
	}
}

func (r planRef) check(kind planKind, staged bool) error {
	old, err := r.load()
	if err != nil {
		return err
	}
	if old.closed || old.plan.invalid {
		return ErrClosed
	}
	if old.plan.kind != kind || old.plan.resolved || (staged && !old.plan.staged) {
		return ErrUsed
	}
	return r.lease.life.check()
}

// Change is one staged original installation; it grants no Snapshot.
type Change struct{ ref planRef }

// ChangeOutcome must be mapped by the future private adapter from the original
// whole durable operation. An error alone is not evidence of rollback.
type ChangeOutcome uint8

// Change outcomes describe the complete original installation operation.
const (
	ChangeUnknown ChangeOutcome = iota
	ConfirmedInstalled
	ConfirmedRollback
)

// BeginChange reserves the scope's only plan before the durable command starts.
func (s *WriteScope) BeginChange() (*Change, error) {
	r, _, err := s.begin(changePlan)
	if err != nil {
		return nil, err
	}
	return &Change{ref: r}, nil
}

// Stage retains exactly one proposed version inside the later original command.
// It accepts no durable authority, and never seals a rolled-back domain.
func (c *Change) Stage(target Mark) error {
	if c == nil {
		return ErrScope
	}
	for {
		old, err := c.ref.load()
		if err != nil {
			return err
		}
		if old.closed || old.plan.invalid {
			return ErrClosed
		}
		if old.plan.resolved || old.plan.staged {
			return c.ref.lease.fail(ErrUsed)
		}
		if err := c.ref.lease.life.check(); err != nil {
			return err
		}
		if !validMark(target) || (old.domain != ([32]byte{}) && old.domain != target.DomainID) {
			return c.ref.lease.fail(ErrInvalid)
		}
		if old.installed != (Mark{}) && target != old.installed && (target.EnvelopeGeneration <= old.installed.EnvelopeGeneration || target.PureGeneration <= old.installed.PureGeneration) {
			return c.ref.lease.fail(ErrMismatch)
		}
		next := *old
		next.plan.target, next.plan.staged = target, true
		if c.ref.lease.gate.state.CompareAndSwap(old, &next) {
			return nil
		}
	}
}

// CheckPrepared is side-effect-free memory/actual-time work for a final hook.
func (c *Change) CheckPrepared() error {
	if c == nil {
		return ErrScope
	}
	return c.ref.check(changePlan, true)
}

// Resolve deliberately does NOT recheck cancellation after an irrevocable
// original effect. It resolves only this prepared still-active scope. Unknown,
// repeated, or invalid results close local admission, never reopen old trust.
func (c *Change) Resolve(outcome ChangeOutcome) error {
	if c == nil {
		return ErrScope
	}
	for {
		old, err := c.ref.load()
		if err != nil {
			return err
		}
		if old.plan.resolved || old.plan.invalid {
			return c.ref.lease.fail(ErrUsed)
		}
		next := *old
		next.plan.resolved = true
		switch outcome {
		case ConfirmedInstalled:
			if !old.plan.staged {
				return c.ref.lease.fail(ErrInvalid)
			}
			target := old.plan.target
			if target != old.installed {
				next.installed, next.domain, next.current = target, target.DomainID, nil
				next.floor = max(old.floor, target.EnvelopeGeneration-1)
			}
			next.plan.effect = InstalledEffect
		case ConfirmedRollback:
			// An independently confirmed no-change cannot undo earlier denial.
		default:
			next.closed, next.current, next.plan.invalid = true, nil, true
			next.plan.effect = UnknownEffect
		}
		if c.ref.lease.gate.state.CompareAndSwap(old, &next) {
			if next.plan.invalid {
				return ErrClosed
			}
			return nil
		}
	}
}

// PublishOutcome reports the original complete publisher's actual result.
type PublishOutcome uint8

// Publication outcomes distinguish confirmed publication from no publication.
const (
	PublishUnknown PublishOutcome = iota
	ConfirmedPublished
	ConfirmedNotPublished
)

// Binding preallocates every node its final Apply might publish. Nothing here
// performs the future source check or the actual complete-publication swap.
type Binding struct {
	ref             planRef
	snapshot        *Snapshot
	prepared        *gateState
	applied         *gateState
	poisonNone      *gateState
	poisonApplied   *gateState
	poisonPublished *gateState
}

// PrepareBinding must run before entering any memory-only publisher callback.
func (s *WriteScope) PrepareBinding(frame Frame) (*Binding, error) {
	r, prepared, err := s.begin(bindingPlan)
	if err != nil {
		return nil, err
	}
	if !validMark(frame.Mark) || frame.OwnerID == ([32]byte{}) {
		return nil, s.lease.fail(ErrInvalid)
	}
	if prepared.installed != frame.Mark || prepared.domain != frame.Mark.DomainID {
		return nil, s.lease.fail(ErrMismatch)
	}
	if frame.Mark.EnvelopeGeneration <= prepared.floor {
		return nil, s.lease.fail(ErrRetired)
	}
	snapshot := prepared.current
	if snapshot != nil && snapshot.frame != frame {
		return nil, s.lease.fail(ErrMismatch)
	}
	if snapshot == nil {
		snapshot = &Snapshot{gate: s.lease.gate, frame: frame}
	}
	applied := *prepared
	applied.current, applied.plan.applied, applied.plan.effect = snapshot, true, PublicationAppliedEffect
	poison := func(effect Effect) *gateState {
		node := *prepared
		node.closed, node.current, node.plan.invalid, node.plan.effect = true, nil, true, effect
		return &node
	}
	return &Binding{ref: r, snapshot: snapshot, prepared: prepared, applied: &applied, poisonNone: poison(UnknownEffect), poisonApplied: poison(PublicationAppliedEffect), poisonPublished: poison(PublishedEffect)}, nil
}

// Snapshot returns a new inactive handle or reuses the exact published handle.
// Holding either handle's bytes/pointer is no admission.
func (b *Binding) Snapshot() *Snapshot {
	if b == nil {
		return nil
	}
	return b.snapshot
}

// Apply is preallocated memory-only: actual clock, cached lifetime signals and
// pointer CAS. No locks, allocation, context-interface calls, I/O or callbacks.
// The embedding publisher must perform all fallible work BEFORE this succeeds,
// then immediately swap the matching complete bundle with no fallible step.
func (b *Binding) Apply() error {
	if b == nil || b.ref.lease == nil {
		return ErrScope
	}
	g := b.ref.lease.gate
	for {
		old := g.state.Load()
		if old.writer != b.ref.lease || old.plan.token != b.ref.token {
			return ErrScope
		}
		if old.closed {
			return ErrClosed
		}
		if old != b.prepared {
			poison := b.poisonNone
			if old.plan.effect == PublicationAppliedEffect {
				poison = b.poisonApplied
			}
			if old.plan.effect == PublishedEffect {
				poison = b.poisonPublished
			}
			if g.state.CompareAndSwap(old, poison) {
				return ErrUsed
			}
			continue
		}
		if err := b.ref.lease.life.check(); err != nil {
			return err
		}
		if g.state.CompareAndSwap(old, b.applied) {
			return nil
		}
	}
}

// Resolve may confirm an already-applied original publication after cancellation.
// A callback error after Apply is uncertain publication, never a safe rollback.
func (b *Binding) Resolve(outcome PublishOutcome) error {
	if b == nil {
		return ErrScope
	}
	for {
		old, err := b.ref.load()
		if err != nil {
			return err
		}
		if old.plan.resolved || old.plan.invalid {
			return b.ref.lease.fail(ErrUsed)
		}
		next := *old
		next.plan.resolved = true
		switch {
		case outcome == ConfirmedPublished && old.plan.applied:
			next.plan.effect = PublishedEffect
		case outcome == ConfirmedNotPublished && !old.plan.applied:
		default:
			next.closed, next.current, next.plan.invalid = true, nil, true
			if next.plan.effect == NoEffect {
				next.plan.effect = UnknownEffect
			}
		}
		if b.ref.lease.gate.state.CompareAndSwap(old, &next) {
			if next.plan.invalid {
				return ErrClosed
			}
			return nil
		}
	}
}

// DenyOutcome never treats a failed denial write as permission for old reads.
type DenyOutcome uint8

// Denial outcomes distinguish durable denial from failed or uncertain writes.
const (
	FailedOrUnknown DenyOutcome = iota
	ConfirmedDurableDeny
)

// Withdrawal prepares one exact installed-version deny before real persistence.
type Withdrawal struct{ ref planRef }

// PrepareWithdrawal does not authenticate revocation or persist any denial.
func (s *WriteScope) PrepareWithdrawal() (*Withdrawal, error) {
	r, old, err := s.begin(withdrawalPlan)
	if err != nil {
		return nil, err
	}
	if old.installed == (Mark{}) {
		return nil, s.lease.fail(ErrInvalid)
	}
	return &Withdrawal{ref: r}, nil
}

// CheckPrepared uses only cached context signals, memory, and actual time.
func (w *Withdrawal) CheckPrepared() error {
	if w == nil {
		return ErrScope
	}
	return w.ref.check(withdrawalPlan, false)
}

// Resolve records already durable denial even if cancellation followed its CAS.
func (w *Withdrawal) Resolve(outcome DenyOutcome) error {
	if w == nil {
		return ErrScope
	}
	for {
		old, err := w.ref.load()
		if err != nil {
			return err
		}
		if old.plan.resolved || old.plan.invalid {
			return w.ref.lease.fail(ErrUsed)
		}
		next := *old
		next.plan.resolved = true
		if outcome == ConfirmedDurableDeny {
			next.floor, next.current = max(old.floor, old.installed.EnvelopeGeneration), nil
			next.plan.effect = DeniedEffect
		} else {
			next.closed, next.current, next.plan.invalid = true, nil, true
			next.plan.effect = UnknownEffect
		}
		if w.ref.lease.gate.state.CompareAndSwap(old, &next) {
			if next.plan.invalid {
				return ErrClosed
			}
			return nil
		}
	}
}
