package sqlite

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// AnchorPublication is one original finite attempt to register input from an
// actual committed managed owner. Copies share consumption. It cannot seal a
// root bundle, admit a response, or replace the complete publisher's own checks.
type AnchorPublication struct{ cell *anchorPublication }

type anchorPublication struct {
	source   *AnchorSource
	cursor   *anchorCursor
	token    GenerationToken
	original context.Context
	life     anchorOriginalLifetime
	attempt  *retirement.WriteAttempt
	used     atomic.Bool
	input    *AnchorResolverInput
	call     atomic.Pointer[anchorPublicationCall]
}

type anchorPublicationPhase uint8

const (
	anchorPublicationArmed anchorPublicationPhase = iota + 1
	anchorPublicationApplying
	anchorPublicationApplied
	anchorPublicationFailed
	anchorPublicationInvalid
	anchorPublicationClosed
)

// All callback-state nodes are preallocated BEFORE the memory-only callback.
// The same pointer CAS arbitrates Apply, duplicate calls and callback return.
// In-flight Apply at return is unknown; it cannot later reopen the registration.
type anchorPublicationCall struct {
	phase anchorPublicationPhase
	nodes *anchorPublicationNodes
}

type anchorPublicationNodes struct {
	binding  *retirement.Binding
	armed    anchorPublicationCall
	applying anchorPublicationCall
	applied  anchorPublicationCall
	failed   anchorPublicationCall
	invalid  anchorPublicationCall
	closed   anchorPublicationCall
}

func newAnchorPublicationNodes(binding *retirement.Binding) *anchorPublicationNodes {
	n := &anchorPublicationNodes{binding: binding}
	n.armed = anchorPublicationCall{phase: anchorPublicationArmed, nodes: n}
	n.applying = anchorPublicationCall{phase: anchorPublicationApplying, nodes: n}
	n.applied = anchorPublicationCall{phase: anchorPublicationApplied, nodes: n}
	n.failed = anchorPublicationCall{phase: anchorPublicationFailed, nodes: n}
	n.invalid = anchorPublicationCall{phase: anchorPublicationInvalid, nodes: n}
	n.closed = anchorPublicationCall{phase: anchorPublicationClosed, nodes: n}
	return n
}

// CaptureAnchorPublication compares the original candidate token with one
// actual committed owner/local/authority/installed cut. The caller must start
// its finite original context (<=30s) BEFORE ReadServingCandidate and retain it
// through compilation/publication; no later capture may rescue an old attempt.
// No proposed State, Projection, Frame, Snapshot or claimed outcome is accepted.
// Awaiting, Dormant, Quarantined and locally denied owners cannot register input.
func (s *Store) CaptureAnchorPublication(ctx context.Context, token GenerationToken) (*AnchorPublication, error) {
	if ctx == nil || token.source != s {
		return nil, replication.ErrInvalid
	}
	if err := s.rejectAnchorReentry(ctx); err != nil {
		return nil, err
	}
	if s.isClosed() {
		return nil, store.ErrClosed
	}
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	source := s.anchors.Load()
	if source == nil || source.closed.Load() || s.anchorBlocked.Load() {
		return nil, ErrAnchorUnavailable
	}
	cursor := source.cursor.Load()
	if !anchorPublicationCursor(cursor) {
		return nil, ErrAnchorUnavailable
	}
	projection, err := resolverstate.Project(cursor.owner)
	if err != nil {
		return nil, err
	}
	if projection.Disposition() != resolverstate.Usable {
		return nil, ErrAnchorUnavailable
	}
	attempt, err := cursor.gate.CaptureWrite(ctx, cursor.mark)
	if err != nil {
		return nil, err
	}
	generation, authority, err := s.captureAnchorBase(ctx, cursor)
	if err != nil {
		return nil, err
	}
	if !generation.Same(token.generation) || authority != anchorAuthority(token.authority) {
		return nil, replication.ErrCAS
	}
	if err := anchorPublicationSource(source, cursor); err != nil {
		return nil, err
	}
	if err := life.check(); err != nil {
		return nil, err
	}
	input := &AnchorResolverInput{cell: &anchorResolverInput{source: source, cursor: cursor, projection: projection}}
	return &AnchorPublication{cell: &anchorPublication{source: source, cursor: cursor, token: token, original: ctx, life: life, attempt: attempt, input: input}}, nil
}

func anchorPublicationCursor(cursor *anchorCursor) bool {
	return cursor != nil && !cursor.awaiting && cursor.gate != nil && cursor.mark != (retirement.Mark{}) && cursor.local != nil && !cursor.local.record.Terminal && cursor.local.record.RetiredThrough < cursor.mark.EnvelopeGeneration
}

// This is a borrowed source predicate, not response authority. Exact cursor
// identity preserves local-only denial and same-byte interposition boundaries.
func anchorPublicationSource(source *AnchorSource, cursor *anchorCursor) error {
	if source == nil || source.store == nil || source.closed.Load() || source.store.isClosed() || source.store.anchorBlocked.Load() || source.store.writeBlocked.Load() {
		return ErrAnchorUnavailable
	}
	if source.store.anchors.Load() != source || source.cursor.Load() != cursor {
		return accepted.ErrCAS
	}
	if !anchorPublicationCursor(cursor) {
		return ErrAnchorUnavailable
	}
	return nil
}

// Projection returns detached structural input from the captured committed cut.
// It is not a root seal, proof of current publication, or a serving grant.
func (p *AnchorPublication) Projection() resolverstate.Projection {
	if p == nil || p.cell == nil {
		return resolverstate.Projection{}
	}
	return p.cell.input.cell.projection
}

// Input returns mechanical source evidence. It has no usable read registration
// until this operation's actual callback applied and returned successfully.
func (p *AnchorPublication) Input() *AnchorResolverInput {
	if p == nil || p.cell == nil {
		return nil
	}
	return p.cell.input
}

// Apply is one preallocated memory-only final step, valid ONLY during the
// synchronous PublishIfCurrent callback. All fallible checks must precede it.
// On success the caller must immediately perform its matching complete-bundle
// seal/swap, with no allocation, callback, context method, I/O or fallible step.
// Returning nil without Apply is refused. Error/panic after Apply closes local
// admission; an error must never be interpreted as proof the bundle was unswapped.
func (p *AnchorPublication) Apply() error {
	if p == nil || p.cell == nil {
		return retirement.ErrScope
	}
	c := p.cell
	for {
		old := c.call.Load()
		if old == nil || old.phase == anchorPublicationClosed {
			return retirement.ErrScope
		}
		n := old.nodes
		if old.phase != anchorPublicationArmed {
			if old.phase == anchorPublicationInvalid || c.call.CompareAndSwap(old, &n.invalid) {
				return retirement.ErrUsed
			}
			continue
		}
		if !c.call.CompareAndSwap(old, &n.applying) {
			continue
		}
		result := anchorPublicationSource(c.source, c.cursor)
		if result == nil {
			result = n.binding.Apply()
		}
		next := &n.applied
		if result != nil {
			next = &n.failed
		}
		if !c.call.CompareAndSwap(&n.applying, next) {
			return retirement.ErrScope
		}
		return result
	}
}

func (s *Store) publishAnchorIfCurrent(ctx context.Context, token GenerationToken, opts PublicationOptions, publish func() error) error {
	p := opts.Anchor
	if p == nil || p.cell == nil || p.cell.source == nil || p.cell.source.store != s {
		return replication.ErrInvalid
	}
	c := p.cell
	if !c.used.CompareAndSwap(false, true) {
		return retirement.ErrUsed
	}
	if !token.generation.Same(c.token.generation) || !c.token.Matches(token.authority) {
		return replication.ErrCAS
	}
	return c.cursor.gate.WithWrite(c.attempt, func(scope *retirement.WriteScope) error {
		// R(write) -> wmu -> original callback. No query takes wmu or uses SQL.
		s.wmu.Lock()
		defer s.wmu.Unlock()
		if err := c.life.check(); err != nil {
			return err
		}
		if err := anchorPublicationSource(c.source, c.cursor); err != nil {
			return err
		}
		generation, authority, err := s.captureAnchorBase(c.original, c.cursor)
		if err != nil {
			return err
		}
		if !generation.Same(c.token.generation) || authority != anchorAuthority(c.token.authority) {
			return replication.ErrCAS
		}
		return s.publishIfCurrentLocked(ctx, token, opts, func() error {
			binding, err := scope.PrepareBinding(retirement.Frame{Mark: c.cursor.mark, OwnerID: c.input.cell.projection.OwnerID()})
			if err != nil {
				return err
			}
			n := newAnchorPublicationNodes(binding)
			c.call.Store(&n.armed)
			return c.invokePublication(n, publish)
		})
	})
}

// Only actual callback/Apply disposition chooses Resolve. No public API accepts
// a caller-claimed outcome. Resolution remains under the SAME original R/wmu.
// Panic propagates after uncertain publication has terminally closed admission.
func (c *anchorPublication) invokePublication(n *anchorPublicationNodes, publish func() error) (result error) {
	returned := false
	defer func() {
		last := c.call.Swap(&n.closed)
		outcome := retirement.PublishUnknown
		if returned && last == &n.applied && result == nil {
			outcome = retirement.ConfirmedPublished
		} else if returned && (last == &n.armed || last == &n.failed) {
			outcome = retirement.ConfirmedNotPublished
			if result == nil {
				result = retirement.ErrScope
			}
		}
		resolution := n.binding.Resolve(outcome)
		if outcome == retirement.ConfirmedPublished && resolution == nil {
			// The original publication work context is deliberately NOT retained
			// by read admission. Each query captures its own original lifetime.
			c.input.cell.snapshot.Store(n.binding.Snapshot())
		}
		if outcome == retirement.PublishUnknown || resolution != nil {
			c.source.store.closeAnchorSource(c.source)
		}
		result = errors.Join(result, resolution)
	}()
	result = publish()
	returned = true
	return result
}

// AnchorResolverInput retains only exact mechanical source/registration input.
// Root's private complete-bundle seal and independent mode/policy/cluster gates
// are mandatory; this type alone cannot authorize DNS or any other output.
// Its retirement Snapshot is private and has no public constructor/accessor.
type AnchorResolverInput struct{ cell *anchorResolverInput }

type anchorResolverInput struct {
	source     *AnchorSource
	cursor     *anchorCursor
	projection resolverstate.Projection
	snapshot   atomic.Pointer[retirement.Snapshot]
}

// AnchorRead retains one input's original finite query context. Copies share
// consumption; a stale response cannot be rescued by another input or capture.
type AnchorRead struct{ cell *anchorRead }

type anchorRead struct {
	input   *anchorResolverInput
	attempt *retirement.ReadAttempt
}

// AnchorReadScope is a borrowed instantaneous predicate within WithRead only.
// Escaping Check or launching output that outlives the callback retains NO hold.
type AnchorReadScope struct {
	input *anchorResolverInput
	scope retirement.ReadScope
}

// CaptureRead must precede the original query's work. Its context must already
// expire within 30s; neither publication cancellation nor a successor context is
// substituted. This allocates no pending gate slot and performs no Store I/O.
func (i *AnchorResolverInput) CaptureRead(ctx context.Context) (*AnchorRead, error) {
	if i == nil || i.cell == nil {
		return nil, replication.ErrInvalid
	}
	if _, err := captureAnchorLifetime(ctx); err != nil {
		return nil, err
	}
	c := i.cell
	if err := anchorPublicationSource(c.source, c.cursor); err != nil {
		return nil, err
	}
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return nil, ErrAnchorUnavailable
	}
	attempt, err := c.cursor.gate.CaptureRead(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	return &AnchorRead{cell: &anchorRead{input: c, attempt: attempt}}, nil
}

// WithRead consumes the exact captured read and holds R through one synchronous
// bounded handoff. Root must independently check its original bundle/policy,
// response proof, cluster and transport limits INSIDE fn. No Store calls, owner
// release, acquisition or work escaping fn belongs there. Gate limits remain
// 256 active/240 pending readers,16 FIFO writers, with writer preference.
func (i *AnchorResolverInput) WithRead(read *AnchorRead, fn func(AnchorReadScope) error) error {
	if i == nil || i.cell == nil || read == nil || read.cell == nil || read.cell.input != i.cell || fn == nil {
		return replication.ErrInvalid
	}
	return i.cell.cursor.gate.WithRead(read.cell.attempt, func(scope retirement.ReadScope) error {
		borrowed := AnchorReadScope{input: i.cell, scope: scope}
		if err := borrowed.Check(); err != nil {
			return err
		}
		if err := fn(borrowed); err != nil {
			return err
		}
		return borrowed.Check()
	})
}

// Check uses only memory, actual time and cached original lifetime signals.
// Its error must be returned unchanged so WithRead preserves cancellation kind.
func (s AnchorReadScope) Check() error {
	if s.input == nil {
		return retirement.ErrScope
	}
	if err := s.scope.Check(); err != nil {
		return err
	}
	return anchorPublicationSource(s.input.source, s.input.cursor)
}
