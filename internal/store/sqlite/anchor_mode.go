package sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

const (
	anchorModeReaders = 256 // TOTAL active plus waiting, independent of R bounds
	anchorModeWriters = 16  // TOTAL active plus waiting
)

// One Store incarnation owns exactly one Static epoch, one Managed epoch and a
// terminal Closed node. Epoch identity never cycles, even for identical bytes.
// Static uses no retirement Mark, history, Snapshot or synthetic registration.
type anchorMode struct {
	weight  *semaphore.Weighted
	state   atomic.Pointer[anchorModeState]
	static  anchorModeState
	managed anchorModeState
	closed  anchorModeState
	mu      sync.Mutex
	readers int
	writers int
	waits   [anchorModeReaders + anchorModeWriters]context.CancelFunc
}

type anchorModeState struct {
	managed  bool
	closed   bool
	writer   *anchorModeScope
	resolved bool
}

type anchorModeAttempt struct {
	mode     *anchorMode
	original context.Context
	life     anchorOriginalLifetime
	managed  bool
	write    bool
	// Derived only from the original captured cursor, never a later binding.
	retirementBound bool
	used            atomic.Bool
}

// Every mutation and scope-close compares the same immutable state pointer.
// The nodes are allocated before invoking any final hook. An escaped Resolve
// cannot change a successor writer after this scope's close linearization.
type anchorModeScope struct {
	attempt     *anchorModeAttempt
	static      anchorModeState
	managed     anchorModeState
	doneStatic  anchorModeState
	doneManaged anchorModeState
	active      atomic.Bool // borrowed reader predicate only; never mutates mode
}

func newAnchorMode(managed bool) *anchorMode {
	m := &anchorMode{weight: semaphore.NewWeighted(anchorModeReaders)}
	m.managed.managed, m.closed.closed = true, true
	if managed {
		m.state.Store(&m.managed)
	} else {
		m.state.Store(&m.static)
	}
	return m
}

func (m *anchorMode) current(managed bool) bool {
	if m == nil {
		return false
	}
	state := m.state.Load()
	return state != nil && !state.closed && state.managed == managed
}

// capture reserves no queue slot and retains only the already bounded ORIGINAL
// lifetime. All arbitrary context methods are called outside our queue locks.
func (m *anchorMode) capture(ctx context.Context, write bool) (*anchorModeAttempt, error) {
	life, err := captureAnchorLifetime(ctx)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrAnchorUnavailable
	}
	state := m.state.Load()
	if state == nil || state.closed {
		return nil, retirement.ErrClosed
	}
	return &anchorModeAttempt{mode: m, original: ctx, life: life, managed: state.managed, write: write}, nil
}

// closeMemory converges without a mutex, context call, wake callback, allocation
// or I/O. Active callbacks keep their semaphore weight until they return.
func (m *anchorMode) closeMemory() {
	if m == nil {
		return
	}
	for {
		old := m.state.Load()
		if old == nil || old.closed || m.state.CompareAndSwap(old, &m.closed) {
			return
		}
	}
}

// wake is bounded work, called only outside final hooks and queue/SQL locks.
// A concurrently registering waiter sees Closed under mu and cancels itself.
func (m *anchorMode) wake() {
	if m == nil {
		return
	}
	var wake [anchorModeReaders + anchorModeWriters]context.CancelFunc
	m.mu.Lock()
	copy(wake[:], m.waits[:])
	m.mu.Unlock()
	for _, cancel := range wake {
		if cancel != nil {
			cancel()
		}
	}
}

func (a *anchorModeAttempt) check() error {
	if a == nil || a.mode == nil {
		return retirement.ErrInvalid
	}
	if err := a.life.check(); err != nil {
		return err
	}
	state := a.mode.state.Load()
	if state == nil || state.closed {
		return retirement.ErrClosed
	}
	if state.managed != a.managed {
		// An original R-bound lane retains its retirement mismatch category;
		// an awaiting first install had no R binding and retains owner CAS.
		if a.retirementBound {
			return retirement.ErrMismatch
		}
		return accepted.ErrCAS
	}
	return nil
}

// admit passes ONLY a private standard-library context to Weighted. Weighted
// can call Err while holding its internal mutex; no original context interface
// or callback is reachable there. The bounded registry counts active callbacks
// too. FIFO/anti-barging is Weighted's enqueue order, not writer preference over
// older readers. Cancellation releases only the caller's own reservation.
func (a *anchorModeAttempt) admit() (func(), error) {
	if a == nil || a.mode == nil {
		return nil, retirement.ErrInvalid
	}
	if !a.used.CompareAndSwap(false, true) {
		return nil, retirement.ErrUsed
	}
	if err := a.check(); err != nil {
		return nil, err
	}
	m := a.mode
	wait, cancel := context.WithDeadline(context.Background(), a.life.deadline)
	m.mu.Lock()
	full := (!a.write && m.readers == anchorModeReaders) || (a.write && m.writers == anchorModeWriters)
	if full || !m.current(a.managed) {
		m.mu.Unlock()
		cancel()
		if full {
			return nil, retirement.ErrCapacity
		}
		if err := a.check(); err != nil {
			return nil, err
		}
		return nil, retirement.ErrClosed
	}
	slot := -1
	for n, pending := range m.waits {
		if pending == nil {
			slot = n
			break
		}
	}
	if slot < 0 {
		m.mu.Unlock()
		cancel()
		return nil, retirement.ErrCapacity
	}
	m.waits[slot] = cancel
	if a.write {
		m.writers++
	} else {
		m.readers++
	}
	m.mu.Unlock()
	// One bounded bridge per admitted attempt; original Done was cached before
	// queue entry. Closing the mode cancels private waits, never active holds.
	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		select {
		case <-a.life.done:
			cancel()
		case <-wait.Done():
		}
	}()
	reserved := func() {
		cancel()
		<-bridgeDone // join before returning this bounded admission slot
		m.mu.Lock()
		m.waits[slot] = nil
		if a.write {
			m.writers--
		} else {
			m.readers--
		}
		m.mu.Unlock()
	}
	weight := int64(1)
	if a.write {
		weight = anchorModeReaders
	}
	err := m.weight.Acquire(wait, weight)
	if err != nil {
		reserved()
		if own := a.check(); own != nil {
			return nil, own
		}
		return nil, err
	}
	release := func() { m.weight.Release(weight); reserved() }
	if err := a.check(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (m *anchorMode) withWrite(a *anchorModeAttempt, fn func(*anchorModeScope) error) (result error) {
	if a == nil || a.mode != m || !a.write || fn == nil {
		return retirement.ErrInvalid
	}
	release, err := a.admit()
	if err != nil {
		return anchorModeOriginalError(a, err)
	}
	defer func() { result = anchorModeOriginalError(a, result) }()
	defer func() {
		if m.state.Load().closed {
			m.wake()
		}
	}()
	defer release()
	scope := &anchorModeScope{attempt: a}
	scope.static.writer = scope
	scope.managed.managed, scope.managed.writer = true, scope
	scope.doneStatic = anchorModeState{writer: scope, resolved: true}
	scope.doneManaged = anchorModeState{writer: scope, managed: true, resolved: true}
	old := m.state.Load()
	node := &scope.static
	if a.managed {
		node = &scope.managed
	}
	if old == nil || old.closed || old.managed != a.managed || old.writer != nil || !m.state.CompareAndSwap(old, node) {
		return retirement.ErrClosed
	}
	entered, returned := false, false
	defer func() {
		// This is the scope-close linearization, BEFORE weight release. A
		// callback that entered must resolve an actual disposition. Returning
		// nil (or an error) is no evidence of no effect. The SAME state CAS
		// arbitrates concurrent resolution: if close wins, no late call can
		// reopen; if resolution wins, close observes that confirmed disposition.
		for {
			old := m.state.Load()
			if old.writer != scope {
				if old.closed && result == nil {
					result = retirement.ErrClosed
				}
				break
			}
			next := &m.static
			if old.managed {
				next = &m.managed
			}
			if entered && (!returned || !old.resolved) {
				next = &m.closed
			}
			if m.state.CompareAndSwap(old, next) {
				if next == &m.closed && result == nil {
					result = retirement.ErrScope
				}
				break
			}
		}
	}()
	// A refusal here precedes callback entry and therefore has known no effect.
	if err := scope.check(); err != nil {
		return err
	}
	entered = true
	result = fn(scope)
	returned = true
	// Completed effects resolve INSIDE fn; post-effect cancellation is not a
	// rollback predicate. Only unlocked outward classification calls Err.
	return result
}

func anchorModeOriginalError(a *anchorModeAttempt, err error) error {
	if a != nil && errors.Is(err, context.Canceled) {
		if original := a.original.Err(); original != nil {
			return original
		}
	}
	return err
}

func (s *anchorModeScope) check() error {
	if s == nil || s.attempt == nil {
		return retirement.ErrScope
	}
	if err := s.attempt.life.check(); err != nil {
		return err
	}
	state := s.attempt.mode.state.Load()
	if state == nil || state.closed || state.writer != s {
		return retirement.ErrScope
	}
	return nil
}

// resolve takes only actual private driver disposition. No public claimed
// outcome can change mode. It intentionally ignores post-effect cancellation.
func (s *anchorModeScope) resolve(managed, unknown bool) error {
	if s == nil || s.attempt == nil {
		return retirement.ErrScope
	}
	m := s.attempt.mode
	for {
		old := m.state.Load()
		if old == nil || old.closed || old.writer != s {
			return retirement.ErrScope
		}
		if old.resolved {
			return retirement.ErrUsed
		}
		if unknown {
			if m.state.CompareAndSwap(old, &m.closed) {
				return retirement.ErrClosed
			}
		} else {
			next := &s.doneStatic
			if managed || old.managed {
				next = &s.doneManaged
			}
			if m.state.CompareAndSwap(old, next) {
				return nil
			}
		}
	}
}

func (s *Store) withAnchorMode(a *anchorModeAttempt, o *anchorOperation, fn func() error) error {
	if o == nil || o.source == nil || a == nil || a.mode != o.source.mode {
		return ErrAnchorUnavailable
	}
	defer func() {
		if state := o.source.mode.state.Load(); state != nil && state.closed {
			s.closeAnchorSource(o.source)
		}
		o.source.flushAnchorClose()
	}()
	return o.source.mode.withWrite(a, func(scope *anchorModeScope) error {
		o.mode = scope
		result := fn()
		// A reported gate-only terminal closure converges conservatively to
		// the common mode. Current observes gate-only closure independently.
		if errors.Is(result, retirement.ErrClosed) {
			s.closeAnchorSource(o.source)
		}
		unknown := o.sql.outcome == AnchorUnknown || o.source.closed.Load()
		// SQL success alone cannot publish a private candidate. Managed mode
		// requires actual target cursor publication after confirmed R resolution.
		managed := false
		if o.sql.outcome == AnchorCommitted && o.target != nil && o.target.mark != (retirement.Mark{}) {
			managed = true
			if o.source.cursor.Load() != o.target {
				unknown = true
			}
		}
		resolution := scope.resolve(managed, unknown)
		if resolution != nil {
			s.closeAnchorSource(o.source)
		}
		return errors.Join(result, resolution)
	})
}

func (s *AnchorSource) flushAnchorClose() {
	if s == nil || !s.closed.Load() {
		return
	}
	s.mode.wake()
	if cursor := s.cursor.Load(); cursor != nil && cursor.gate != nil {
		cursor.gate.Close()
	}
	s.notify()
}

// AnchorStaticInput is mechanical evidence for the actual Store's Static
// epoch. It supplies no roots, bundle seal, config eligibility or output grant.
// Managed history can never create it; proposal bytes cannot choose its epoch.
type AnchorStaticInput struct {
	source *AnchorSource
	mode   *anchorMode
}

// StaticInput observes bound committed history. Root must separately refuse
// static/default policy whenever Managed is configured or historically present.
func (s *AnchorSource) StaticInput() (*AnchorStaticInput, error) {
	if s == nil || s.store == nil || s.store.anchors.Load() != s || s.closed.Load() || s.store.isClosed() || s.store.anchorBlocked.Load() || !s.mode.current(false) {
		return nil, ErrAnchorUnavailable
	}
	if cursor := s.cursor.Load(); cursor == nil || cursor.mark != (retirement.Mark{}) {
		return nil, ErrAnchorUnavailable
	}
	return &AnchorStaticInput{source: s, mode: s.mode}, nil
}

// Current is a borrowed instantaneous predicate; it holds no output exclusion.
func (i *AnchorStaticInput) Current() bool {
	if i == nil || i.source == nil {
		return false
	}
	cursor := i.source.cursor.Load()
	return cursor != nil && cursor.mark == (retirement.Mark{}) && i.source.mode == i.mode && i.source.store.anchors.Load() == i.source && !i.source.closed.Load() && !i.source.store.isClosed() && !i.source.store.anchorBlocked.Load() && !i.source.store.writeBlocked.Load() && i.mode.current(false)
}

// AnchorStaticRead is a one-use original query attempt, not a serving permit.
type AnchorStaticRead struct {
	input   *AnchorStaticInput
	attempt *anchorModeAttempt
}

// AnchorStaticScope is borrowed only until its synchronous callback returns.
type AnchorStaticScope struct {
	input *AnchorStaticInput
	scope *anchorModeScope
}

// CaptureRead precedes original query work and retains its finite <=30s life.
func (i *AnchorStaticInput) CaptureRead(ctx context.Context) (*AnchorStaticRead, error) {
	if !i.Current() {
		return nil, ErrAnchorUnavailable
	}
	a, err := i.mode.capture(ctx, false)
	if err != nil {
		return nil, err
	}
	if a.managed || !i.Current() {
		return nil, ErrAnchorUnavailable
	}
	return &AnchorStaticRead{input: i, attempt: a}, nil
}

func (m *anchorMode) withRead(a *anchorModeAttempt, fn func(*anchorModeScope) error) (result error) {
	if a == nil || a.mode != m || a.write || fn == nil {
		return replication.ErrInvalid
	}
	release, err := a.admit()
	if err != nil {
		return anchorModeOriginalError(a, err)
	}
	defer func() { result = anchorModeOriginalError(a, result) }()
	defer release()
	scope := &anchorModeScope{attempt: a}
	scope.active.Store(true)
	defer scope.active.Store(false)
	if err := scope.readCheck(); err != nil {
		return err
	}
	if err := fn(scope); err != nil {
		return err
	}
	return scope.readCheck()
}

func (s *anchorModeScope) readCheck() error {
	if s == nil || !s.active.Load() {
		return retirement.ErrScope
	}
	return s.attempt.check()
}

// WithRead holds mode through the actual synchronous handoff. No Store calls,
// publisher pins, new acquisition or output escaping fn belongs inside it.
func (i *AnchorStaticInput) WithRead(read *AnchorStaticRead, fn func(AnchorStaticScope) error) error {
	if read == nil || read.input != i || i == nil || fn == nil {
		return replication.ErrInvalid
	}
	return i.mode.withRead(read.attempt, func(scope *anchorModeScope) error {
		borrowed := AnchorStaticScope{input: i, scope: scope}
		if err := borrowed.Check(); err != nil {
			return err
		}
		if err := fn(borrowed); err != nil {
			return err
		}
		return borrowed.Check()
	})
}

// Check is instantaneous; escaping it never retains exclusion after return.
func (s AnchorStaticScope) Check() error {
	if s.scope == nil {
		return retirement.ErrScope
	}
	if err := s.scope.readCheck(); err != nil {
		return err
	}
	if !s.input.Current() {
		return ErrAnchorUnavailable
	}
	return nil
}

// Capture composes the original epoch with the exact immutable source cursor;
// it cannot reinterpret a cursor published during an unresolved mode change.
func captureAnchorMode(ctx context.Context, source *AnchorSource, cursor *anchorCursor, write bool) (*anchorModeAttempt, error) {
	if source == nil || cursor == nil {
		return nil, ErrAnchorUnavailable
	}
	a, err := source.mode.capture(ctx, write)
	if err != nil {
		return nil, err
	}
	a.retirementBound = cursor.gate != nil
	if a.managed != (cursor.mark != (retirement.Mark{})) || source.closed.Load() || source.store.anchors.Load() != source || source.cursor.Load() != cursor {
		return nil, accepted.ErrCAS
	}
	return a, nil
}

func (s *Store) flushAnchorSourceClosed() {
	if source := s.anchors.Load(); source != nil {
		source.flushAnchorClose()
	}
}
