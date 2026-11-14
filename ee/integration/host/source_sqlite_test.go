package host

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var sourceSQLiteValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

// Real SQLite writer authority, runtime owners and Host barriers. The test
// callback stages only a small section probe: production source mutation and
// current target admission belong to the separate root adapter slice.
func sourceSQLiteFixture(t *testing.T) (*Host, *job, *sqlite.Store, sqlite.WriterToken) {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "source.db"), sqlite.Options{Identity: "source-host", Scheme: scheme, Validator: sourceSQLiteValidator(), ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	h, j, _ := preferredSourceFixture(t)
	in := j.frame.input
	in.Term = s.Authority().Term
	instances, err := CaptureInstances(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	g, err := instances[0].Guard("original-source-lease")
	if err != nil {
		t.Fatal(err)
	}
	j.frame = frame{instances[0], in}
	j.guard = g
	j.leaseCtx = sqlite.WithWriterToken(j.leaseCtx, token)
	j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) { return SourceSnapshot{Complete: true}, nil }}
	h.opts.Reader = s
	h.opts.Ledger, err = ledger.New(ledger.Options{Reader: s, Write: func(ctx context.Context, g ledger.Guard, obj store.Object, opts store.ApplyOptions) (store.Object, error) {
		var out store.Object
		err := h.WithLease(ctx, g, func(ctx context.Context) error { var err error; out, err = s.Apply(ctx, obj, opts); return err })
		return out, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.BeforeObserve = func(ctx, originalLease context.Context, g ExecutionGuard) (context.Context, func(), error) {
		original, ok := sqlite.WriterTokenFromContext(originalLease)
		if !ok || original != token || !original.Term().Same(g.Ledger.Term) {
			t.Fatal("binder did not receive original elector token")
		}
		return sqlite.WithWriterToken(ctx, original), func() {}, nil
	}
	return h, j, s, token
}

func TestActualSQLiteSourceCommandOnceRollbackFinalAndNoop(t *testing.T) {
	for _, mode := range []string{"commit", "callback_rollback", "final_write", "final_noop", "deadline", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			h, j, s, token := sourceSQLiteFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				// The callback waits out the bound inside the transaction; the
				// bound must outlast the work before it on a loaded machine
				// (see TestActualSQLiteSourceCommittedPendingSurvivesAckDeadline).
				h.opts.OperationTimeout = 2 * time.Second
			}
			var allowed atomic.Bool
			allowed.Store(true)
			h.opts.Allowed = allowed.Load
			before, err := s.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			binds, releases, commands, finals := 0, 0, 0, 0
			originalBinder := h.opts.BeforeObserve
			h.opts.BeforeObserve = func(ctx, lease context.Context, g ExecutionGuard) (context.Context, func(), error) {
				binds++
				if sqlite.HasWriterToken(ctx) {
					t.Fatal("manual context unexpectedly sampled writer")
				}
				bound, release, err := originalBinder(ctx, lease, g)
				return bound, func() { releases++; release() }, err
			}
			sentinel := errors.New("rollback probe")
			h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
				c, err := h.SourceCapture(ctx, g)
				if err != nil {
					return nil, err
				}
				if got, ok := sqlite.WriterTokenFromContext(ctx); !ok || got != token {
					t.Fatal("commit sampled successor writer")
				}
				err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
					return s.Command(ctx, g.Ledger.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
						commands++
						if tx.Incarnation() != token.Incarnation() || !tx.Authority().Same(token.Term()) {
							t.Fatal("actual transaction did not admit original writer")
						}
						if err := tx.RequireCommitCheck(func(ctx context.Context) error { finals++; return final(ctx) }); err != nil {
							return err
						}
						if mode != "final_noop" {
							data, _ := json.Marshal(map[string]string{"request": c.RequestID()})
							if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "source-host-probe", Data: data}); err != nil {
								return err
							}
						}
						switch mode {
						case "callback_rollback":
							return sentinel
						case "final_write", "final_noop":
							allowed.Store(false)
						case "deadline":
							<-time.After(time.Until(c.Deadline()) + time.Millisecond)
						case "cancel":
							cancel()
						}
						return nil
					})
				})
				if releases != 0 {
					t.Fatal("original root proof released before commit/ack")
				}
				if err != nil {
					return &Result{Applied: 7, Clients: 1, Source: &SourceOutcome{}}, err
				}
				return &Result{Source: &SourceOutcome{ReceiptID: c.RequestID(), Committed: true, Published: true}}, nil
			}
			a := h.execute(ctx, j, command{kind: "sync"})
			after, err := s.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if commands != 1 || binds != 1 || releases != 1 {
				t.Fatal("callback/binder repeated", commands, binds, releases)
			}
			if mode == "commit" {
				if a.err != nil || !a.result.Source.Committed || !a.result.Source.Published || after.Revision != before.Revision+1 || len(after.Payload.Dedup) != 1 || finals != 1 {
					t.Fatal("actual commit incomplete", a, finals, after.Revision, before.Revision)
				}
			} else {
				if a.err == nil || a.result == nil || a.result.Source.Committed || a.result.Applied != 0 || a.result.Clients != 0 || after.Revision != before.Revision || after.PayloadSHA256 != before.PayloadSHA256 || len(after.Payload.Dedup) != 0 {
					t.Fatal("rollback escaped/reported success", a, after.Revision, before.Revision)
				}
				if mode == "callback_rollback" && !errors.Is(a.err, sentinel) {
					t.Fatal("callback error not preserved", a.err)
				}
				if (mode == "final_write" || mode == "final_noop") && finals != 1 {
					t.Fatal("final omitted", finals)
				}
			}
		})
	}
}

func TestActualSQLiteSourceCommittedPendingSurvivesAckDeadline(t *testing.T) {
	h, j, s, _ := sourceSQLiteFixture(t)
	// The bound covers everything up to the SQLite commit as well as the
	// simulated acknowledgment wait. It must outlast the work before the
	// commit on a loaded machine: a 40 ms bound failed on hosted CI when it
	// ran out before the commit, which then rightly committed nothing. The
	// wait below always runs into the bound, so the asserted outcome is
	// unchanged.
	h.opts.OperationTimeout = 2 * time.Second
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	binder := h.opts.BeforeObserve
	h.opts.BeforeObserve = func(ctx, lease context.Context, g ExecutionGuard) (context.Context, func(), error) {
		bound, release, err := binder(ctx, lease, g)
		return bound, func() { released = true; release() }, err
	}
	h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
		c, err := h.SourceCapture(ctx, g)
		if err != nil {
			return nil, err
		}
		err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
			return s.Command(ctx, g.Ledger.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.RequireCommitCheck(final); err != nil {
					return err
				}
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "source-host-probe", Data: json.RawMessage(`{"committed":true}`)})
			})
		})
		if err != nil {
			return nil, err
		}
		<-ctx.Done() // Simulated root complete-bundle acknowledgment wait.
		if released {
			t.Fatal("root proof released before acknowledgment ended")
		}
		return &Result{Source: &SourceOutcome{ReceiptID: c.RequestID(), Committed: true}}, ctx.Err()
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	after, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.err, context.DeadlineExceeded) || a.result == nil || !a.result.Source.Committed || a.result.Source.Published || a.result.Pending != 1 || !released || after.Revision != before.Revision+1 || len(after.Payload.Dedup) != 1 {
		t.Fatal("ack timeout lost durable outcome", a, after.Revision, before.Revision)
	}
}

func TestActualSQLiteSourceWaitsWithOriginalCancellationAndWriterToken(t *testing.T) {
	for _, mode := range []string{"caller_cancel", "lease_cancel", "writer_replaced"} {
		t.Run(mode, func(t *testing.T) {
			h, j, s, token := sourceSQLiteFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lease, leaseCancel := context.WithCancel(j.leaseCtx)
			defer leaseCancel()
			j.leaseCtx = lease
			before, err := s.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var commands int
			h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
				c, err := h.SourceCapture(ctx, g)
				if err != nil {
					return nil, err
				}
				if got, ok := sqlite.WriterTokenFromContext(ctx); !ok || got != token {
					t.Fatal("writer was rebound")
				}
				err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
					return s.Command(ctx, g.Ledger.Term, func(_ context.Context, tx *sqlite.CommandTx) error { commands++; return tx.RequireCommitCheck(final) })
				})
				return nil, err
			}
			if mode == "writer_replaced" {
				j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(context.Context) (SourceSnapshot, error) {
					next := s.Authority().Term
					next.Epoch++
					next.PrimaryID = "other-primary"
					next.PromotionID = "other-promotion"
					if _, err := s.ObserveAuthority(t.Context(), next); err != nil {
						t.Fatal(err)
					}
					return SourceSnapshot{Complete: true}, nil
				}}
				a := h.execute(ctx, j, command{kind: "sync"})
				if !errors.Is(a.err, replication.ErrFenced) || commands != 0 {
					t.Fatal("old writer was replaced", a, commands)
				}
			} else {
				entered, release := make(chan struct{}), make(chan struct{})
				blocker := make(chan error, 1)
				go func() {
					blocker <- s.Command(sqlite.WithWriterToken(t.Context(), token), token.Term(), func(context.Context, *sqlite.CommandTx) error { close(entered); <-release; return nil })
				}()
				<-entered
				waiting := make(chan struct{})
				var original context.Context
				commit := h.opts.CommitSource
				h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, s SourceSnapshot) (*Result, error) {
					original = ctx
					close(waiting)
					return commit(ctx, g, s)
				}
				done := make(chan answer, 1)
				go func() { done <- h.execute(ctx, j, command{kind: "sync"}) }()
				<-waiting
				if mode == "caller_cancel" {
					cancel()
				} else {
					leaseCancel()
				}
				// The host links a lease to its operation through
				// context.AfterFunc, so the cancellation reaches the waiting
				// command a goroutine hop after leaseCancel returns. Release
				// the writer only once the original operation context has
				// observed it; a command entered inside that hop is still
				// refused at commit by the synchronous lease check of final.
				select {
				case <-original.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("original cancellation never reached the waiting command")
				}
				close(release)
				if err := <-blocker; err != nil {
					t.Fatal(err)
				}
				a := <-done
				if a.err == nil || commands != 0 {
					t.Fatal("canceled original writer entered command", a, commands)
				}
			}
			after, err := s.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision || len(after.Payload.Dedup) != 0 {
				t.Fatal("fenced operation changed durable data")
			}
		})
	}
}

// heldLease is a lease context whose cancellation is visible through Err and
// Done at once, while every context.AfterFunc registered on it (and so every
// context derived from it) runs only at flush. It holds the goroutine hop
// between a lease cancel and the operation context seeing it open.
type heldLease struct {
	context.Context
	mu       sync.Mutex
	done     chan struct{}
	canceled bool
	held     []func()
}

func newHeldLease(parent context.Context) *heldLease {
	return &heldLease{Context: parent, done: make(chan struct{})}
}

func (l *heldLease) Done() <-chan struct{} { return l.done }

func (l *heldLease) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.canceled {
		return context.Canceled
	}
	return nil
}

func (l *heldLease) cancel() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.canceled {
		l.canceled = true
		close(l.done)
	}
}

// AfterFunc makes context.AfterFunc and context.WithCancel register here.
func (l *heldLease) AfterFunc(f func()) func() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := len(l.held)
	l.held = append(l.held, f)
	return func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.held[i] == nil {
			return false
		}
		l.held[i] = nil
		return true
	}
}

func (l *heldLease) flush() {
	l.mu.Lock()
	held := l.held
	l.held = make([]func(), len(held))
	l.mu.Unlock()
	for _, f := range held {
		if f != nil {
			f()
		}
	}
}

// A command that enters its transaction after the lease was canceled but
// before the cancellation reached the operation context (the AfterFunc hop)
// is refused at commit by final's synchronous lease check, and writes nothing.
func TestActualSQLiteSourceLeaseCancelBeforePropagationFencesAtCommit(t *testing.T) {
	h, j, s, token := sourceSQLiteFixture(t)
	lease := newHeldLease(j.leaseCtx)
	defer lease.flush()
	j.leaseCtx = lease
	before, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var commands atomic.Int32
	var operation context.Context
	h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
		c, err := h.SourceCapture(ctx, g)
		if err != nil {
			return nil, err
		}
		err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
			operation = ctx
			close(waiting)
			return s.Command(ctx, g.Ledger.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				commands.Add(1)
				if err := tx.RequireCommitCheck(final); err != nil {
					return err
				}
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "source-host-probe", Data: json.RawMessage(`{"committed":true}`)})
			})
		})
		return nil, err
	}
	entered, release := make(chan struct{}), make(chan struct{})
	blocker := make(chan error, 1)
	go func() {
		blocker <- s.Command(sqlite.WithWriterToken(t.Context(), token), token.Term(), func(context.Context, *sqlite.CommandTx) error { close(entered); <-release; return nil })
	}()
	<-entered
	done := make(chan answer, 1)
	go func() { done <- h.execute(context.Background(), j, command{kind: "sync"}) }()
	<-waiting
	lease.cancel()
	if operation.Err() != nil {
		t.Fatal("the operation context saw the cancel before the held hop")
	}
	close(release)
	if err := <-blocker; err != nil {
		t.Fatal(err)
	}
	a := <-done
	if !errors.Is(a.err, ErrFenced) || commands.Load() != 1 {
		t.Fatal("a command entered inside the cancel hop was not fenced at commit", a, commands.Load())
	}
	after, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || len(after.Payload.Dedup) != 0 {
		t.Fatal("a command fenced at commit changed durable data", after.Revision, before.Revision, len(after.Payload.Dedup))
	}
}

func TestActualSQLiteSourceRequestIDSurvivesHTTPAttemptBudgets(t *testing.T) {
	h, j, s, token := sourceSQLiteFixture(t)
	var original SourceCapture
	attempts := 0
	j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(ctx context.Context) (SourceSnapshot, error) {
		_, g, err := h.captured(j)
		if err != nil {
			return SourceSnapshot{}, err
		}
		original, err = h.SourceCapture(ctx, g)
		if err != nil {
			return SourceSnapshot{}, err
		}
		for range 2 {
			err = (providerHost{h, j}).Request(ctx, "test-source", "account", false, func(attemptCtx context.Context) error {
				attempts++
				got, err := h.SourceCapture(attemptCtx, g)
				if err != nil || got.RequestID() != original.RequestID() || !got.Deadline().Equal(original.Deadline()) {
					t.Fatal("HTTP retry renewed observation scope", got, err)
				}
				if got, ok := sqlite.WriterTokenFromContext(attemptCtx); !ok || got != token {
					t.Fatal("HTTP attempt lost original writer")
				}
				return nil
			})
			if err != nil {
				return SourceSnapshot{}, err
			}
		}
		return SourceSnapshot{Complete: true}, nil
	}}
	h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
		c, err := h.SourceCapture(ctx, g)
		if err != nil {
			return nil, err
		}
		if c.RequestID() != original.RequestID() {
			t.Fatal("commit renewed observation ID")
		}
		err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
			return s.Command(ctx, g.Ledger.Term, func(_ context.Context, tx *sqlite.CommandTx) error { return tx.RequireCommitCheck(final) })
		})
		if err != nil {
			return nil, err
		}
		return &Result{Source: &SourceOutcome{ReceiptID: c.RequestID(), Committed: true, Published: true}}, nil
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	if a.err != nil || attempts != 2 {
		t.Fatal(a.err, attempts)
	}
}
