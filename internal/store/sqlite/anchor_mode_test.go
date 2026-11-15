package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// Compose the parent's actual schema8 fixture helpers; no copied dependencies,
// backward markers, late source capture or weakened complete-owner validation.
func anchorModeTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func anchorModeTestWait(ctx context.Context, t *testing.T, predicate func() bool) {
	t.Helper()
	timer := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	for !predicate() {
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatal("original bounded fixture did not converge", ctx.Err())
		}
	}
}

func anchorModeTestResult(ctx context.Context, t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("original bounded callback did not join", ctx.Err())
		return ctx.Err()
	}
}

func anchorModeTestCapture(ctx context.Context, t *testing.T, m *anchorMode, write bool) *anchorModeAttempt {
	t.Helper()
	a, err := m.capture(ctx, write)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func anchorModeTestCounts(m *anchorMode, readers, writers int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readers == readers && m.writers == writers
}

func anchorModeTestQueued(m *anchorMode) bool {
	// With a held single reader, ample capacity remains. TryAcquire fails only
	// after an enqueued weight256 writer; it cannot barge ahead of that writer.
	if m.weight.TryAcquire(1) {
		m.weight.Release(1)
		return false
	}
	return true
}

func anchorModeTestStaticHold(ctx context.Context, t *testing.T, input *AnchorStaticInput) (func(), <-chan error) {
	t.Helper()
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(leave) }) }
	t.Cleanup(release)
	go func() {
		done <- input.WithRead(read, func(scope AnchorStaticScope) error {
			close(entered)
			select {
			case <-leave:
			case <-ctx.Done():
			}
			return scope.Check()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("counted static handoff did not enter")
	}
	return release, done
}

func TestAnchorModeBindDerivesCommittedHistoryAndNeverRearms(t *testing.T) {
	primary := openTemp(t, internalOptions)
	source, dormant := anchorLocalTestDormant(t, primary)
	input, err := source.StaticInput()
	if err != nil || !input.Current() || !source.mode.current(false) {
		t.Fatal("actual Dormant lacks static epoch", err)
	}
	again, err := primary.BindAnchorLifecycle(anchorTestContext(t.Context(), t, primary))
	if err != nil || again != source || again.mode != source.mode {
		t.Fatal("Bind replaced mode incarnation", err)
	}
	anchorTestManaged(t, primary, dormant)
	if input.Current() || source.mode.current(false) || !source.mode.current(true) {
		t.Fatal("completed bootstrap did not retire static")
	}
	if _, err := source.StaticInput(); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Managed fabricated static input", err)
	}
	primary.closeAnchorSource(source)
	source.flushAnchorClose()
	if _, err := primary.BindAnchorLifecycle(anchorTestContext(t.Context(), t, primary)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Bind rearmed terminal mode", err)
	}
}

// Actual SQLite bootstrap plus a counted synchronous handoff callback. This
// fixture performs no native/HTTP/DoQ transport write or cache/edge operation.
func TestAnchorModeFirstPrimaryBootstrapSQLDrainsCountedStaticHandoff(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, dormant := anchorLocalTestDormant(t, s)
	input, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorTestContext(t.Context(), t, s)
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant) // signed acquisition AFTER capture
	release, readDone := anchorModeTestStaticHold(ctx, t, input)
	commandDone := make(chan error, 1)
	called := atomic.Bool{}
	go func() {
		completion, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
			called.Store(true)
			if err := proof.observation.CheckBootstrap(proof.state, proof.clock.Sample()); err != nil {
				return err
			}
			if err := tx.RequireCommitCheck(func(context.Context) error { return proof.observation.CheckLifetime(proof.clock.Sample()) }); err != nil {
				return err
			}
			return anchorTestStage(ctx, tx, proof.next)
		})
		if err == nil && completion.Outcome() != AnchorCommitted {
			err = errors.New("missing actual bootstrap commit")
		}
		commandDone <- err
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(source.mode) })
	if called.Load() || source.cursor.Load().owner.Version() != dormant.Version() || !input.Current() {
		t.Fatal("bootstrap bypassed held static handoff")
	}
	release()
	if err := anchorModeTestResult(ctx, t, readDone); err != nil {
		t.Fatal(err)
	}
	if err := anchorModeTestResult(ctx, t, commandDone); err != nil {
		t.Fatal(err)
	}
	if !called.Load() || input.Current() || !source.mode.current(true) || source.cursor.Load().owner.Version() != proof.next.Version() {
		t.Fatal("first actual Managed cut did not invalidate static")
	}
}

// Actual SQLite follower install plus a counted synchronous handoff callback;
// this is lock/SQL composition coverage, not real transport output qualification.
func TestAnchorModeFirstFollowerInstallSQLDrainsCountedStaticHandoff(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	input, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorModeTestContext(t)
	captured, err := follower.CaptureAnchorInstall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
	image, err := primary.ReadGeneration(captured) // source acquisition AFTER capture
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(captured, *image)
	if err != nil {
		t.Fatal(err)
	}
	release, readDone := anchorModeTestStaticHold(ctx, t, input)
	done := make(chan error, 1)
	go func() {
		_, err := follower.InstallReplica(captured, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed})
		done <- err
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(source.mode) })
	if !source.cursor.Load().awaiting || source.cursor.Load().gate != nil || !input.Current() {
		t.Fatal("private first-install candidate bypassed static drain")
	}
	release()
	if err := anchorModeTestResult(ctx, t, readDone); err != nil {
		t.Fatal(err)
	}
	if err := anchorModeTestResult(ctx, t, done); err != nil {
		t.Fatal(err)
	}
	cursor := source.cursor.Load()
	if input.Current() || !source.mode.current(true) || cursor.owner.Version() != proof.next.Version() || cursor.gate == nil || !sameAnchorLocal(cursor.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("confirmed first follower cut failed to retire static")
	}
	if cursor.local.record.Carrier == anchorLocalTestRead(t, primary).record.Carrier {
		t.Fatal("first install copied sender's local carrier")
	}
}

func TestAnchorModeFIFOAntiBargingAndIndependentWriterCapacity(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	hold := anchorModeTestCapture(ctx, t, m, false)
	release, err := hold.admit()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { release() }()
	writers := make([]*anchorModeAttempt, anchorModeWriters)
	for n := range writers {
		writers[n] = anchorModeTestCapture(ctx, t, m, true)
	}
	order := make(chan string, 2)
	writerLeave := make(chan struct{})
	var leaveOnce sync.Once
	unblock := func() { leaveOnce.Do(func() { close(writerLeave) }) }
	defer unblock()
	done := make(chan error, anchorModeWriters+1)
	writerContexts := make([]context.CancelFunc, 0, anchorModeWriters-1)
	defer func() {
		for _, cancel := range writerContexts {
			cancel()
		}
	}()
	go func() {
		done <- m.withWrite(writers[0], func(scope *anchorModeScope) error {
			order <- "writer"
			select {
			case <-writerLeave:
			case <-ctx.Done():
			}
			return scope.resolve(false, false)
		})
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(m) })
	// A later reader has a reserved admission slot but cannot bypass the FIFO writer.
	later := anchorModeTestCapture(ctx, t, m, false)
	go func() { done <- m.withRead(later, func(*anchorModeScope) error { order <- "reader"; return nil }) }()
	for n := 1; n < anchorModeWriters; n++ {
		original, cancel := context.WithCancel(ctx)
		writerContexts = append(writerContexts, cancel)
		a := anchorModeTestCapture(original, t, m, true)
		go func() {
			done <- m.withWrite(a, func(scope *anchorModeScope) error { return scope.resolve(false, false) })
		}()
	}
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestCounts(m, 2, anchorModeWriters) })
	extra := anchorModeTestCapture(ctx, t, m, true)
	if err := m.withWrite(extra, func(scope *anchorModeScope) error {
		t.Error("overflow writer entered")
		return scope.resolve(false, false)
	}); !errors.Is(err, retirement.ErrCapacity) {
		t.Fatal("writer bound exceeded", err)
	}
	// Release exactly once; the deferred cleanup remains safe on every failure.
	release()
	release = func() {}
	select {
	case got := <-order:
		if got != "writer" {
			t.Fatal("reader barged", got)
		}
	case <-ctx.Done():
		t.Fatal("writer never entered")
	}
	select {
	case got := <-order:
		t.Fatal("reader bypassed active writer", got)
	default:
	}
	for _, cancel := range writerContexts {
		cancel()
	}
	unblock()
	for range anchorModeWriters + 1 {
		err := anchorModeTestResult(ctx, t, done)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	select {
	case got := <-order:
		if got != "reader" {
			t.Fatal("FIFO reader did not resume", got)
		}
	case <-ctx.Done():
		t.Fatal("reader never resumed")
	}
	if !anchorModeTestCounts(m, 0, 0) {
		t.Fatal("capacity reservations leaked")
	}
}

func TestAnchorModeTotalReaderBoundIncludesActiveAndWaiting(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	writer := anchorModeTestCapture(ctx, t, m, true)
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(leave) }) }
	defer unblock()
	done := make(chan error, anchorModeReaders+1)
	go func() {
		done <- m.withWrite(writer, func(scope *anchorModeScope) error {
			close(entered)
			select {
			case <-leave:
			case <-ctx.Done():
			}
			return scope.resolve(false, false)
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("writer did not enter")
	}
	original, cancel := context.WithCancel(ctx)
	defer cancel()
	for range anchorModeReaders {
		a := anchorModeTestCapture(original, t, m, false)
		go func() {
			done <- m.withRead(a, func(*anchorModeScope) error { t.Error("cancelled waiter entered"); return nil })
		}()
	}
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestCounts(m, anchorModeReaders, 1) })
	overflow := anchorModeTestCapture(ctx, t, m, false)
	if err := m.withRead(overflow, func(*anchorModeScope) error { return nil }); !errors.Is(err, retirement.ErrCapacity) {
		t.Fatal("total reader bound exceeded", err)
	}
	cancel()
	for range anchorModeReaders {
		if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, context.Canceled) {
			t.Fatal("queued reader lost original cancel", err)
		}
	}
	if !anchorModeTestCounts(m, 0, 1) {
		t.Fatal("cancelled waiters retained capacity")
	}
	unblock()
	if err := anchorModeTestResult(ctx, t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAnchorModeOriginalLifetimeNoRecaptureAndReuse(t *testing.T) {
	m := newAnchorMode(false)
	if _, err := m.capture(context.Background(), false); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("unbounded mode capture", err)
	}
	long, cancelLong := context.WithTimeout(t.Context(), 31*time.Second)
	defer cancelLong()
	if _, err := m.capture(long, false); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long original silently capped", err)
	}
	ctx, cancel := context.WithCancel(anchorModeTestContext(t))
	a := anchorModeTestCapture(ctx, t, m, true)
	cancel()
	if err := m.withWrite(a, func(scope *anchorModeScope) error {
		t.Error("cancelled original entered")
		return scope.resolve(false, false)
	}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := m.withWrite(a, func(scope *anchorModeScope) error { return scope.resolve(false, false) }); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("failed attempt reusable", err)
	}
	foreign := newAnchorMode(false)
	fresh := anchorModeTestCapture(anchorModeTestContext(t), t, m, true)
	if err := foreign.withWrite(fresh, func(scope *anchorModeScope) error { return scope.resolve(false, false) }); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("foreign mode accepted", err)
	}
}

func TestAnchorModeScopeCloseResolveAndABAUseOneCAS(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	var old *anchorModeScope
	err := m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		old = scope
		if err := scope.resolve(true, false); err != nil {
			return err
		}
		if err := scope.resolve(false, false); !errors.Is(err, retirement.ErrUsed) {
			t.Error("resolved plan mutable", err)
		}
		return nil
	})
	if err != nil || !m.current(true) {
		t.Fatal("monotone resolve failed", err)
	}
	err = m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		if err := old.resolve(false, true); !errors.Is(err, retirement.ErrScope) {
			t.Error("old scope changed successor", err)
		}
		if err := old.check(); !errors.Is(err, retirement.ErrScope) {
			t.Error("old scope remained active", err)
		}
		return scope.resolve(false, false) // Managed can never resolve to Static
	})
	if err != nil || !m.current(true) {
		t.Fatal("ABA or downgrade changed mode", err)
	}
}

func TestAnchorModeConcurrentResolutionAndReturnCannotMutateSuccessor(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	var escaped *anchorModeScope
	start, done := make(chan struct{}), make(chan error, 1)
	err := m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		escaped = scope
		go func() { <-start; done <- escaped.resolve(true, false) }()
		return scope.resolve(false, false) // actual known no effect before return
	})
	if err != nil {
		t.Fatal(err)
	}
	err = m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		close(start)
		if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, retirement.ErrScope) {
			t.Error("escaped scope mutated active successor", err)
		}
		return scope.resolve(false, false)
	})
	if err != nil || !m.current(false) {
		t.Fatal("successor state corrupted", err)
	}
}

func TestAnchorModeCancelCloseNeverReleaseActiveCallback(t *testing.T) {
	for _, closeSource := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "close"}[closeSource], func(t *testing.T) {
			m, outer := newAnchorMode(false), anchorModeTestContext(t)
			ctx, cancel := context.WithCancel(outer)
			defer cancel()
			a := anchorModeTestCapture(ctx, t, m, false)
			entered, leave := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(leave) }) }
			defer release()
			done := make(chan error, 1)
			var escaped *anchorModeScope
			go func() {
				done <- m.withRead(a, func(scope *anchorModeScope) error { escaped = scope; close(entered); <-leave; return scope.readCheck() })
			}()
			select {
			case <-entered:
			case <-outer.Done():
				t.Fatal("reader not active")
			}
			if closeSource {
				m.closeMemory()
				m.wake()
			} else {
				cancel()
			}
			if m.weight.TryAcquire(anchorModeReaders) {
				m.weight.Release(anchorModeReaders)
				t.Fatal("cancel/close forcibly released active reader")
			}
			if !anchorModeTestCounts(m, 1, 0) {
				t.Fatal("active callback reservation lost")
			}
			release()
			if err := anchorModeTestResult(outer, t, done); err == nil {
				t.Fatal("stopped read reported success")
			}
			if err := escaped.readCheck(); !errors.Is(err, retirement.ErrScope) {
				t.Fatal("borrowed scope escaped return", err)
			}
			if !anchorModeTestCounts(m, 0, 0) {
				t.Fatal("returned reader leaked reservation")
			}
		})
	}
}

func TestAnchorModeMemoryCloseWakesBoundedWaitersOutsideHook(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	active := anchorModeTestCapture(ctx, t, m, false)
	release, err := active.admit()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	writer := anchorModeTestCapture(ctx, t, m, true)
	go func() {
		done <- m.withWrite(writer, func(scope *anchorModeScope) error {
			t.Error("closed writer entered")
			return scope.resolve(false, false)
		})
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(m) })
	// Deliberately hold mu while closing: closeMemory cannot need it, enqueue
	// wake callbacks, call contexts or attempt to return the active weight.
	m.mu.Lock()
	m.closeMemory()
	m.mu.Unlock()
	m.wake()
	if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, retirement.ErrClosed) {
		t.Fatal("closed wait lost refusal", err)
	}
	if !anchorModeTestCounts(m, 1, 0) {
		t.Fatal("close released somebody else's reader")
	}
}

// An original custom context is allowed to report DeadlineExceeded when Done
// closes before wall time. It must never be invoked from Weighted's mutex or
// final hooks. Arm after original capture; outward Err is intentionally allowed.
type anchorModeTestContextProbe struct {
	context.Context
	armed   atomic.Bool
	methods atomic.Int32
}

func (c *anchorModeTestContextProbe) Done() <-chan struct{} {
	if c.armed.Load() {
		c.methods.Add(1)
	}
	return c.Context.Done()
}
func (c *anchorModeTestContextProbe) Deadline() (time.Time, bool) {
	if c.armed.Load() {
		c.methods.Add(1)
	}
	return c.Context.Deadline()
}
func (c *anchorModeTestContextProbe) Err() error {
	if c.armed.Load() {
		c.methods.Add(1)
	}
	return c.Context.Err()
}

func TestAnchorModePrivateWaitAndFinalHooksUseOnlyCachedSignals(t *testing.T) {
	m := newAnchorMode(false)
	ctx := &anchorModeTestContextProbe{Context: anchorModeTestContext(t)}
	a := anchorModeTestCapture(ctx, t, m, true)
	ctx.armed.Store(true)
	if err := m.withWrite(a, func(scope *anchorModeScope) error {
		if err := scope.check(); err != nil {
			return err
		}
		return scope.resolve(false, false)
	}); err != nil {
		t.Fatal(err)
	}
	if ctx.methods.Load() != 0 {
		t.Fatal("queue/final hook called original context interface")
	}
}

func TestAnchorModeSQLRollbackAndCommitUnknownHaveActualDisposition(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "trigger-rollback", true: "deferred-commit-unknown"}[unknown], func(t *testing.T) {
			s := openTemp(t, internalOptions)
			source, dormant := anchorLocalTestDormant(t, s)
			input, err := source.StaticInput()
			if err != nil {
				t.Fatal(err)
			}
			statements := []string{`CREATE TEMP TRIGGER anchor_mode_abort BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture mode rollback'); END`}
			if unknown {
				statements = []string{
					`CREATE TEMP TABLE anchor_mode_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE anchor_mode_child(parent INTEGER REFERENCES anchor_mode_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER anchor_mode_unknown AFTER UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN INSERT INTO anchor_mode_child(parent) VALUES (1); END`,
				}
			}
			for _, statement := range statements {
				if _, err := s.w.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			ctx := anchorTestContext(t.Context(), t, s)
			update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
			if err != nil {
				t.Fatal(err)
			}
			proof := anchorTestBootstrap(t, s, dormant)
			before, local := source.cursor.Load(), anchorLocalTestRead(t, s)
			reached := false
			completion, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
				reached = true
				return anchorTestStage(ctx, tx, proof.next)
			})
			var failure interface{ Code() int }
			if !reached || !errors.As(err, &failure) {
				t.Fatal("SQL target not reached", err)
			}
			if unknown {
				if failure.Code() != 787 || completion.Outcome() != AnchorUnknown || !errors.Is(err, sql.ErrTxDone) || input.Current() || !source.mode.state.Load().closed {
					t.Fatal("real failed Commit reopened static", err)
				}
			} else {
				if failure.Code() != 1811 || completion.Outcome() != AnchorRolledBack || !input.Current() || !source.mode.current(false) || source.closed.Load() {
					t.Fatal("confirmed precommit rollback closed static", err)
				}
			}
			if source.cursor.Load() != before || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
				t.Fatal("fault credited noncommitted owner/local postimage")
			}
		})
	}
}

func TestAnchorModeConfirmedCommitSurvivesPostEffectCancellation(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, dormant := anchorLocalTestDormant(t, s)
	ctx, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant)
	original := source.cursor.Load()
	o := &anchorOperation{source: source, before: original, sql: anchorSQL{outcome: AnchorRolledBack}}
	// Same actual private command body/resolution as AnchorCommand; cancellation
	// is injected after confirmed SQL Commit, not by a fake claimed outcome.
	err = s.withAnchorMode(update.cell.mode, o, func() error {
		return original.gate.WithWrite(update.cell.attempt, func(scope *retirement.WriteScope) error {
			change, err := scope.BeginChange()
			if err != nil {
				return err
			}
			o.scope, o.change = scope, change
			result := s.command(ctx, update.cell.writer.Term(), func(ctx context.Context, tx *CommandTx) error { return anchorTestStage(ctx, tx, proof.next) }, o)
			if result != nil {
				return result
			}
			if !o.sql.committed || o.sql.outcome != AnchorCommitted {
				return errors.New("actual SQL boundary not committed")
			}
			cancel()
			return o.resolve()
		})
	})
	if err != nil || o.receipt().Outcome() != AnchorCommitted || !source.mode.current(true) || source.cursor.Load() != o.target {
		t.Fatal("completed effect reclassified as rollback", err)
	}
}

func TestAnchorModeStaticReadOriginalIdentityAndBorrowedLifetime(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, _ := anchorLocalTestDormant(t, s)
	left, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	right, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorModeTestContext(t)
	read, err := left.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := right.WithRead(read, func(AnchorStaticScope) error { return nil }); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign input adopted original read", err)
	}
	var saved AnchorStaticScope
	if err := left.WithRead(read, func(scope AnchorStaticScope) error { saved = scope; return scope.Check() }); err != nil {
		t.Fatal(err)
	}
	if err := saved.Check(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("escaped borrowed static scope remained active", err)
	}
	if err := left.WithRead(read, func(AnchorStaticScope) error { return nil }); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("static read reusable", err)
	}
}

func TestAnchorModeManagedReadHoldsModeBeforeRetirementAndCommand(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorTestContext(t.Context(), t, s)
	input := anchorPublicationTestPublish(ctx, t, s)
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(leave) }) }
	defer unblock()
	done := make(chan error, 2)
	go func() {
		done <- input.WithRead(read, func(scope AnchorReadScope) error {
			close(entered)
			select {
			case <-leave:
			case <-ctx.Done():
			}
			return scope.Check()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("managed read not entered")
	}
	update, err := s.CaptureAnchorUpdate(ctx, proof.next.Version())
	if err != nil {
		t.Fatal(err)
	}
	called := atomic.Bool{}
	go func() {
		_, err := s.AnchorCommand(update, func(context.Context, *CommandTx) error { called.Store(true); return nil })
		done <- err
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(source.mode) })
	if called.Load() || !anchorModeTestCounts(source.mode, 1, 1) {
		t.Fatal("command bypassed managed output mode")
	}
	unblock()
	for range 2 {
		if err := anchorModeTestResult(ctx, t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAnchorModeKnownDenialIsIndependentAndPreservesPairedPhase2(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorTestContext(t.Context(), t, s)
	poll, err := s.CaptureAnchorPoll(ctx, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	c := poll.cell
	if c.denial.mode == c.normal.mode || c.normal.mode == c.second.cell.mode || c.denial.mode == c.second.cell.mode {
		t.Fatal("poll branches share consumed mode attempt")
	}
	for _, a := range []*anchorModeAttempt{c.denial.mode, c.normal.mode, c.second.cell.mode} {
		if a.original != ctx || a.mode != source.mode || !a.managed {
			t.Fatal("poll recaptured original operation cut")
		}
	}
	receipt := anchorPollTestReceive(t, poll, proof, true)
	completion, next, err := s.CommitAnchorPollDenial(receipt)
	if err != nil || completion.Outcome() != AnchorCommitted || next == nil || !source.mode.current(true) {
		t.Fatal("actual local deny failed", err)
	}
	completion, err = s.AnchorPollCommandAfterDenial(next, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	})
	if err != nil || completion.Outcome() != AnchorCommitted || !source.mode.current(true) || source.cursor.Load().owner.Version() == proof.next.Version() {
		t.Fatal("own effect phase2 failed", err)
	}
}

func TestAnchorModeInertPublicationAndUnrelatedCommandNeverAcquireMode(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, _ := anchorLocalTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(leave) }) }
	defer unblock()
	done := make(chan error, 1)
	attempt := anchorModeTestCapture(ctx, t, source.mode, true)
	go func() {
		done <- source.mode.withWrite(attempt, func(scope *anchorModeScope) error {
			close(entered)
			select {
			case <-leave:
			case <-ctx.Done():
			}
			return scope.resolve(false, false)
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("mode writer did not enter")
	}
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { called = true; return nil }); err != nil || !called {
		t.Fatal("nil Anchor unexpectedly entered mode", err)
	}
	if err := s.Command(ctx, s.Authority().Term, func(context.Context, *CommandTx) error { return nil }); err != nil {
		t.Fatal("unrelated command serialized on mode", err)
	}
	unblock()
	if err := anchorModeTestResult(ctx, t, done); err != nil {
		t.Fatal(err)
	}
}

func TestAnchorModeRawAwaitingInstallAndRequestNoopKeepStatic(t *testing.T) {
	primary, follower, source := anchorAwaitingTestPair(t)
	input, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		ctx := anchorTestInstallContext(t.Context(), t, follower)
		image := anchorAwaitingTestImage(ctx, t, primary, follower)
		if _, err := follower.InstallReplica(ctx, image, anchorAwaitingTestOptions(ctx, t)); err != nil {
			t.Fatal(err)
		}
		if !input.Current() || !source.mode.current(false) || source.cursor.Load().gate != nil {
			t.Fatal("raw non-granting install fabricated Managed history")
		}
	}
	_, receiver, rawSource, validated, req, _ := anchorAwaitingTestResolved(t)
	ctx := anchorTestInstallContext(t.Context(), t, receiver)
	req.ExpectedInstalled = ctx.Value(anchorInstallKey{}).(*anchorInstall).installed
	if _, err := receiver.ResolveConflict(ctx, validated, req); err != nil {
		t.Fatal("captured RequestID no-op refused", err)
	}
	if !rawSource.mode.current(false) {
		t.Fatal("RequestID no-op minted Managed mode")
	}
}

func TestAnchorModeManagedReopenKeepsHistoryAndStaticRefusal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mode-reopen")
	s := openAt(t, dir, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, dir, internalOptions)
	source, err := reopened.BindAnchorLifecycle(anchorTestContext(t.Context(), t, reopened))
	if err != nil || !source.mode.current(true) || source.cursor.Load().owner.Version() != proof.next.Version() {
		t.Fatal("reopen did not derive actual Managed mode", err)
	}
	if _, err := source.StaticInput(); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("restart reset Managed to static", err)
	}
}

func TestAnchorModeStoreCloseKeepsActualOutputHoldUntilReturn(t *testing.T) {
	s := openTemp(t, internalOptions)
	source, _ := anchorLocalTestDormant(t, s)
	input, err := source.StaticInput()
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorModeTestContext(t)
	release, done := anchorModeTestStaticHold(ctx, t, input)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if input.Current() || !source.mode.state.Load().closed || !anchorModeTestCounts(source.mode, 1, 0) {
		t.Fatal("Store.Close reset mode or released active output")
	}
	release()
	if err := anchorModeTestResult(ctx, t, done); err == nil {
		t.Fatal("closed output reported admission")
	}
}

func TestAnchorModePublicZeroInputsRefuse(t *testing.T) {
	var input *AnchorStaticInput
	if input.Current() {
		t.Fatal("zero static current")
	}
	if _, err := input.CaptureRead(anchorModeTestContext(t)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal(err)
	}
	if err := (AnchorStaticScope{}).Check(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal(err)
	}
	var source *AnchorSource
	if _, err := source.StaticInput(); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal(err)
	}
}

func TestAnchorModeStaleStaticWriteCannotAdoptManagedSuccessor(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	old := anchorModeTestCapture(ctx, t, m, true)
	next := anchorModeTestCapture(ctx, t, m, true)
	if err := m.withWrite(next, func(scope *anchorModeScope) error { return scope.resolve(true, false) }); err != nil {
		t.Fatal(err)
	}
	if err := m.withWrite(old, func(scope *anchorModeScope) error {
		t.Error("stale static writer entered successor")
		return scope.resolve(false, false)
	}); !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("mode transition silently reinterpreted original attempt", err)
	}
	if !m.current(true) {
		t.Fatal("stale write downgraded Managed")
	}
}

type anchorModeTestDeadlineContext struct {
	context.Context
	done <-chan struct{}
}

func (c anchorModeTestDeadlineContext) Done() <-chan struct{} { return c.done }
func (c anchorModeTestDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func TestAnchorModeOriginalCancellationCategorySurvivesPrivateWait(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	hold := anchorModeTestCapture(ctx, t, m, false)
	release, err := hold.admit()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stopped := make(chan struct{})
	original := anchorModeTestDeadlineContext{Context: ctx, done: stopped}
	a := anchorModeTestCapture(original, t, m, true)
	done := make(chan error, 1)
	go func() {
		done <- m.withWrite(a, func(scope *anchorModeScope) error {
			t.Error("stopped writer entered")
			return scope.resolve(false, false)
		})
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(m) })
	close(stopped)
	if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatal("private wait replaced original deadline category", err)
	}
	if !anchorModeTestCounts(m, 1, 0) {
		t.Fatal("stopped writer leaked reservation")
	}
}

func TestAnchorModeOriginalWallDeadlineAndPanicClose(t *testing.T) {
	t.Run("wall-deadline", func(t *testing.T) {
		m := newAnchorMode(false)
		original, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		a := anchorModeTestCapture(original, t, m, false)
		<-original.Done()
		if err := m.withRead(a, func(*anchorModeScope) error { t.Error("expired original entered"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
	t.Run("panic", func(t *testing.T) {
		m, ctx := newAnchorMode(false), anchorModeTestContext(t)
		func() {
			defer func() {
				if recover() == nil {
					t.Error("callback panic swallowed")
				}
			}()
			_ = m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(*anchorModeScope) error { panic("mode fixture") })
		}()
		if !m.state.Load().closed || !anchorModeTestCounts(m, 0, 0) {
			t.Fatal("unknown panic reopened mode or leaked active hold")
		}
	})
}

func TestAnchorModeMixedActiveAndPendingReadersShareTotalBound(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	active := anchorModeTestCapture(ctx, t, m, false)
	release, err := active.admit()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	original, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, anchorModeReaders)
	writer := anchorModeTestCapture(original, t, m, true)
	go func() {
		done <- m.withWrite(writer, func(scope *anchorModeScope) error {
			t.Error("cancelled queued writer entered")
			return scope.resolve(false, false)
		})
	}()
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestQueued(m) })
	for range anchorModeReaders - 1 {
		a := anchorModeTestCapture(original, t, m, false)
		go func() {
			done <- m.withRead(a, func(*anchorModeScope) error { t.Error("later reader barged ahead of queued writer"); return nil })
		}()
	}
	anchorModeTestWait(ctx, t, func() bool { return anchorModeTestCounts(m, anchorModeReaders, 1) })
	overflow := anchorModeTestCapture(ctx, t, m, false)
	if err := m.withRead(overflow, func(*anchorModeScope) error { return nil }); !errors.Is(err, retirement.ErrCapacity) {
		t.Fatal("active plus pending readers exceeded total bound", err)
	}
	cancel()
	for range anchorModeReaders {
		if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled mixed queue changed category", err)
		}
	}
	if !anchorModeTestCounts(m, 1, 0) {
		t.Fatal("mixed queue leaked or released active hold")
	}
}

func TestAnchorModeFirstFollowerSQLFailurePreservesActualCandidateDisposition(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit-unknown"}[unknown], func(t *testing.T) {
			primary, _, _ := anchorLocalTestManaged(t)
			follower, source := anchorLocalTestFollower(t, primary)
			input, err := source.StaticInput()
			if err != nil {
				t.Fatal(err)
			}
			statements := []string{`CREATE TEMP TRIGGER anchor_mode_install_abort BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture mode install rollback'); END`}
			if unknown {
				statements = []string{
					`CREATE TEMP TABLE anchor_mode_install_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE anchor_mode_install_child(parent INTEGER REFERENCES anchor_mode_install_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER anchor_mode_install_unknown AFTER UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN INSERT INTO anchor_mode_install_child(parent) VALUES (1); END`,
				}
			}
			for _, statement := range statements {
				if _, err := follower.w.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			ctx, err := follower.CaptureAnchorInstall(anchorModeTestContext(t))
			if err != nil {
				t.Fatal(err)
			}
			cell := ctx.Value(anchorInstallKey{}).(*anchorInstall)
			image, err := primary.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			validated, err := follower.ValidateImage(ctx, *image)
			if err != nil {
				t.Fatal("fixture refused before SQL target", err)
			}
			before, local := source.cursor.Load(), anchorLocalTestRead(t, follower)
			var actual *anchorOperation
			// Inspect actual private SQL disposition under the same public
			// dispatch/body as InstallReplica. Public entry is covered above.
			err = follower.withAnchorInstall(ctx, validated.Image(), func(ctx context.Context, o *anchorOperation) error {
				actual = o
				_, err := follower.installReplica(ctx, validated.Image(), replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed}, o)
				return err
			})
			var failure interface{ Code() int }
			if actual == nil || actual.mode == nil || actual.scope == nil || actual.sql.tx == nil || !errors.As(err, &failure) {
				t.Fatal("first install did not reach mode/R/SQL target", err)
			}
			if unknown {
				if failure.Code() != 787 || !actual.sql.attempted || actual.sql.committed || actual.sql.outcome != AnchorUnknown || input.Current() || !source.mode.state.Load().closed {
					t.Fatal("unknown first follower commit reopened static", err)
				}
			} else {
				if failure.Code() != 1811 || actual.sql.attempted || actual.sql.outcome != AnchorRolledBack || !input.Current() || !source.mode.current(false) {
					t.Fatal("real first follower rollback lost static epoch", err)
				}
			}
			if source.cursor.Load() != before || source.cursor.Load().gate != nil || !sameAnchorLocal(local, anchorLocalTestRead(t, follower)) {
				t.Fatal("failed first install published private candidate")
			}
		})
	}
}

func TestAnchorModePublicationRetainsModeAndWorkCancellationDoesNotRetireInput(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	original, cancel := context.WithCancel(anchorModeTestContext(t))
	defer cancel()
	token, p := anchorPublicationTestCapture(original, t, s)
	var saved *anchorModeScope
	err := s.PublishIfCurrent(original, token, PublicationOptions{Anchor: p}, func() error {
		state := source.mode.state.Load()
		if state == nil || state.writer == nil || !anchorModeTestCounts(source.mode, 0, 1) {
			t.Fatal("publication callback lacks mode writer")
		}
		saved = state.writer
		if err := p.Apply(); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil || !p.Input().Current() || !source.mode.current(true) {
		t.Fatal("completed registration inherited publication cancellation", err)
	}
	if err := saved.check(); !errors.Is(err, context.Canceled) && !errors.Is(err, retirement.ErrScope) {
		t.Fatal("escaped publication mode remained live", err)
	}
	anchorPublicationTestRead(anchorModeTestContext(t), t, p.Input())
}

func TestAnchorModeKnownRevokeCancellationClosesWithoutDurableCredit(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	original, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	denial, err := s.CaptureAnchorDenial(original, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	before, local := source.cursor.Load(), anchorLocalTestRead(t, s)
	cancel()
	completion, own, err := s.CommitAnchorDenial(verified)
	if !errors.Is(err, context.Canceled) || completion.Outcome() != AnchorRolledBack || own != nil || !source.mode.state.Load().closed || !source.closed.Load() {
		t.Fatal("cancelled known revoke reopened mode or claimed durable denial", err)
	}
	if source.cursor.Load() != before || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
		t.Fatal("cancelled denial credited uncommitted bytes")
	}
}

func TestAnchorModeWriterCancelCloseKeepsWeightUntilCallbackReturn(t *testing.T) {
	m, outer := newAnchorMode(false), anchorModeTestContext(t)
	original, cancel := context.WithCancel(outer)
	defer cancel()
	a := anchorModeTestCapture(original, t, m, true)
	entered, leave := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(leave) }) }
	defer release()
	done := make(chan error, 1)
	go func() {
		done <- m.withWrite(a, func(scope *anchorModeScope) error { close(entered); <-leave; return scope.check() })
	}()
	select {
	case <-entered:
	case <-outer.Done():
		t.Fatal("writer not entered")
	}
	cancel()
	m.closeMemory()
	m.wake()
	if m.weight.TryAcquire(1) {
		m.weight.Release(1)
		t.Fatal("cancel/close released active writer weight")
	}
	if !anchorModeTestCounts(m, 0, 1) {
		t.Fatal("active writer reservation lost")
	}
	release()
	if err := anchorModeTestResult(outer, t, done); err == nil {
		t.Fatal("stopped writer reported admission")
	}
	if !anchorModeTestCounts(m, 0, 0) {
		t.Fatal("writer hold leaked after callback return")
	}
}

func TestAnchorModeFirstFollowerConfirmedCommitSurvivesPostEffectCancel(t *testing.T) {
	primary, _, _ := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	original, cancel := context.WithCancel(anchorModeTestContext(t))
	defer cancel()
	captured, err := follower.CaptureAnchorInstall(original)
	if err != nil {
		t.Fatal(err)
	}
	cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
	image, err := primary.ReadGeneration(captured)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(captured, *image)
	if err != nil {
		t.Fatal(err)
	}
	var actual *anchorOperation
	err = follower.withAnchorInstall(captured, validated.Image(), func(ctx context.Context, o *anchorOperation) error {
		actual = o
		_, err := follower.installReplica(ctx, validated.Image(), replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed}, o)
		if err != nil {
			return err
		}
		if !o.sql.committed || o.sql.outcome != AnchorCommitted || source.cursor.Load() != cell.cursor {
			return errors.New("actual unresolved first commit boundary not reached")
		}
		cancel()
		return nil // outer original scope resolves this actual completion
	})
	if err != nil || actual == nil || actual.sql.outcome != AnchorCommitted || source.cursor.Load() != actual.target || !source.mode.current(true) {
		t.Fatal("first installed effect cancelled back to Static", err)
	}
	if !sameAnchorLocal(actual.target.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("first confirmed install published different local bytes")
	}
}

func TestAnchorModeEnteredUnresolvedReturnClosesTerminally(t *testing.T) {
	for _, callbackError := range []bool{false, true} {
		t.Run(map[bool]string{false: "nil-return", true: "error-return"}[callbackError], func(t *testing.T) {
			m, ctx := newAnchorMode(false), anchorModeTestContext(t)
			refused := errors.New("fixture callback refused without resolving")
			var escaped *anchorModeScope
			err := m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
				escaped = scope
				if callbackError {
					return refused
				}
				return nil // intentionally unresolved: neither return is rollback evidence
			})
			if err == nil || !m.state.Load().closed || !anchorModeTestCounts(m, 0, 0) {
				t.Fatal("entered unresolved return preserved original mode", err)
			}
			if callbackError && !errors.Is(err, refused) {
				t.Fatal("original callback error lost", err)
			}
			if err := escaped.resolve(true, false); !errors.Is(err, retirement.ErrScope) {
				t.Fatal("escaped resolution reopened terminal mode", err)
			}
			if _, err := m.capture(ctx, true); !errors.Is(err, retirement.ErrClosed) {
				t.Fatal("unresolved close was rearmed", err)
			}
		})
	}
}

func TestAnchorModeUnresolvedReturnBeatsEscapedConcurrentResolve(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	start, done := make(chan struct{}), make(chan error, 1)
	err := m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		go func() { <-start; done <- scope.resolve(true, false) }()
		return nil // scope-close is guaranteed to win before escaped call starts
	})
	if err == nil || !m.state.Load().closed {
		t.Fatal("unresolved close did not linearize")
	}
	close(start)
	if err := anchorModeTestResult(ctx, t, done); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("late concurrent resolve reopened closed mode", err)
	}
	if !m.state.Load().closed || !anchorModeTestCounts(m, 0, 0) {
		t.Fatal("escaped call mutated terminal state or hold")
	}
}

func TestAnchorModeConfirmedConcurrentResolveBeatsReturn(t *testing.T) {
	m, ctx := newAnchorMode(false), anchorModeTestContext(t)
	err := m.withWrite(anchorModeTestCapture(ctx, t, m, true), func(scope *anchorModeScope) error {
		done := make(chan error, 1)
		go func() { done <- scope.resolve(true, false) }()
		return anchorModeTestResult(ctx, t, done) // resolution wins same-state CAS first
	})
	if err != nil || !m.current(true) || !anchorModeTestCounts(m, 0, 0) {
		t.Fatal("confirmed resolution lost to scope return", err)
	}
}

func TestAnchorModePublicationSettlesActualCallbackDisposition(t *testing.T) {
	for _, disposition := range []string{"refusal-before-Apply", "nil-without-Apply", "error-after-Apply"} {
		t.Run(disposition, func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			ctx := anchorModeTestContext(t)
			token, p := anchorPublicationTestCapture(ctx, t, s)
			refused := errors.New("fixture actual publication disposition")
			applied := false
			err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
				if disposition == "nil-without-Apply" {
					return nil
				}
				if disposition == "error-after-Apply" {
					if err := p.Apply(); err != nil {
						return err
					}
					applied = true
				}
				return refused
			})
			if err == nil {
				t.Fatal("refused callback reported success")
			}
			if disposition == "error-after-Apply" {
				var actual *retirement.EffectError
				if !applied || !errors.Is(err, refused) || !errors.As(err, &actual) || actual.Effect() != retirement.PublicationAppliedEffect || !source.mode.state.Load().closed {
					t.Fatal("unknown applied effect preserved mode or lost actual effect", err)
				}
			} else if source.mode.state.Load().closed || !source.mode.current(true) || source.closed.Load() {
				t.Fatal("confirmed not-published callback left mode unresolved", err)
			}
			if !anchorModeTestCounts(source.mode, 0, 0) {
				t.Fatal("publication writer leaked")
			}
		})
	}
}

func TestAnchorModePublicationPreCallbackRefusalAndGateOnlyClosure(t *testing.T) {
	for _, gateClosed := range []bool{false, true} {
		t.Run(map[bool]string{false: "interposed-generation", true: "gate-only-closed"}[gateClosed], func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			ctx := anchorTestContext(t.Context(), t, s)
			old := anchorPublicationTestPublish(ctx, t, s)
			token, p := anchorPublicationTestCapture(ctx, t, s)
			if gateClosed {
				source.cursor.Load().gate.Close()
			} else {
				anchorPublicationTestMutation(ctx, t, s)
			}
			called := false
			err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
			if called {
				t.Fatal("pre-callback refusal reached publication")
			}
			if gateClosed {
				if !errors.Is(err, retirement.ErrClosed) || old.Current() || !source.closed.Load() || !source.mode.state.Load().closed {
					t.Fatal("gate-only closure did not converge mode", err)
				}
			} else {
				if !errors.Is(err, replication.ErrCAS) || !source.mode.current(true) || source.closed.Load() || !old.Current() {
					t.Fatal("known pre-callback no effect closed mode", err)
				}
			}
		})
	}
}

// This call-context wrapper injects a synchronous panic only at the existing
// publishIfCurrentLocked Err check under mode/R/wmu. The captured publication,
// SQL and both original gate lifetimes retain the unchanged original context.
// No callback is added to a final hook or queue mutex.
type anchorModeTestPublicationContext struct {
	context.Context
	source       *AnchorSource
	marker       error
	observedOpen atomic.Bool
}

func (c *anchorModeTestPublicationContext) Err() error {
	if state := c.source.mode.state.Load(); state != nil && state.writer != nil {
		c.observedOpen.Store(!state.closed && !c.source.closed.Load() && !c.source.store.anchorBlocked.Load())
		panic(c.marker)
	}
	return c.Context.Err()
}

func TestAnchorModePublicationPreCallbackPanicConvergesClosedSource(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	original := anchorModeTestContext(t)
	token, p := anchorPublicationTestCapture(original, t, s)
	// The coalesced channel has at most one old startup/bootstrap notice.
	select {
	case <-source.Changes():
	default:
	}
	marker := errors.New("fixture publication panic before binding callback")
	call := &anchorModeTestPublicationContext{Context: original, source: source, marker: marker}
	called := false
	recovered := func() (value any) {
		defer func() { value = recover() }()
		_ = s.PublishIfCurrent(call, token, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
		return nil
	}()
	if recovered != marker || !call.observedOpen.Load() || called || p.cell.call.Load() != nil {
		t.Fatal("fixture missed open source before actual publication callback", recovered)
	}
	if !source.mode.state.Load().closed || !source.closed.Load() || !s.anchorBlocked.Load() || !anchorModeTestCounts(source.mode, 0, 0) {
		t.Fatal("strict mode panic failed to converge source admission after release")
	}
	if !s.wmu.TryLock() {
		t.Fatal("publication panic retained SQL writer lock")
	}
	s.wmu.Unlock()
	select {
	case <-source.Changes():
	default:
		t.Fatal("post-unwind source closure did not notify")
	}
	if p.Input().Current() {
		t.Fatal("panic left mechanical source current")
	}
}
