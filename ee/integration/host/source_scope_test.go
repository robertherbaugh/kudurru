package host

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSourceCaptureOriginalRequestIdentityAndLifetime(t *testing.T) {
	h, j, snapshot := preferredSourceFixture(t)
	type originalKey struct{}
	j.leaseCtx = context.WithValue(j.leaseCtx, originalKey{}, "original-lease")
	var acquisition SourceCapture
	var saved context.Context
	var final func(context.Context) error
	ids := map[string]bool{}
	binds, releases, polls, commits, callbacks := 0, 0, 0, 0, 0
	h.opts.BeforeObserve = func(ctx, originalLease context.Context, g ExecutionGuard) (context.Context, func(), error) {
		binds++
		if originalLease.Value(originalKey{}) != "original-lease" || ctx.Value(originalKey{}) != "caller" {
			t.Fatal("lost original lease/caller values")
		}
		c, err := h.SourceCapture(ctx, g)
		if err != nil || c.RequestID() == "" || !c.Guard().Ledger.Term.Same(g.Ledger.Term) {
			t.Fatal(c, err)
		}
		if ids[c.RequestID()] {
			t.Fatal("new observation reused request ID")
		}
		ids[c.RequestID()] = true
		acquisition = c
		return context.WithValue(ctx, originalKey{}, originalLease.Value(originalKey{})), func() { releases++ }, nil
	}
	j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(ctx context.Context) (SourceSnapshot, error) {
		polls++
		if ctx.Value(originalKey{}) != "original-lease" {
			t.Fatal("binder proof missing during acquisition")
		}
		c, err := h.SourceCapture(ctx, acquisition.Guard())
		if err != nil || c.RequestID() != acquisition.RequestID() || !c.Deadline().Equal(acquisition.Deadline()) {
			t.Fatal("acquisition recaptured scope", c, err)
		}
		return snapshot, nil
	}}
	h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
		commits++
		c, err := h.SourceCapture(ctx, g)
		if err != nil || c.RequestID() != acquisition.RequestID() || ctx.Value(originalKey{}) != "original-lease" {
			t.Fatal(c, err)
		}
		wrong := g
		wrong.BundleID = "successor"
		if _, err = h.SourceCapture(ctx, wrong); !errors.Is(err, ErrFenced) {
			t.Fatal("capture adopted successor", err)
		}
		if _, err = h.SourceCapture(context.Background(), g); !errors.Is(err, ErrFenced) {
			t.Fatal("capture fabricated from guard", err)
		}
		saved = ctx
		err = h.WithSource(ctx, c, func(ctx context.Context, f func(context.Context) error) error {
			callbacks++
			final = f
			if ctx.Value(originalKey{}) != "original-lease" {
				t.Fatal("commit lost binder context")
			}
			return f(ctx)
		})
		if err != nil {
			t.Fatal(err)
		}
		if err = final(ctx); !errors.Is(err, ErrFenced) {
			t.Fatal("retained final remained valid", err)
		}
		if err = h.WithSource(ctx, c, func(context.Context, func(context.Context) error) error { t.Fatal("callback repeated"); return nil }); !errors.Is(err, ErrFenced) {
			t.Fatal(err)
		}
		return &Result{Source: &SourceOutcome{ReceiptID: c.RequestID(), Committed: true, Published: true}}, nil
	}
	for range 2 {
		a := h.execute(context.WithValue(context.Background(), originalKey{}, "caller"), j, command{kind: "sync"})
		if a.err != nil {
			t.Fatal(a.err)
		}
		if _, err := h.SourceCapture(saved, acquisition.Guard()); !errors.Is(err, ErrFenced) {
			t.Fatal("ended observation captured", err)
		}
		if err := h.WithSource(saved, acquisition, func(context.Context, func(context.Context) error) error {
			t.Fatal("ended callback admitted")
			return nil
		}); !errors.Is(err, ErrFenced) {
			t.Fatal(err)
		}
	}
	if binds != 2 || releases != 2 || polls != 2 || commits != 2 || callbacks != 2 {
		t.Fatal(binds, releases, polls, commits, callbacks)
	}
}

func TestSourceOriginalScopeRejectsChangesAfterAcquisition(t *testing.T) {
	for _, point := range []string{"observe", "commit"} {
		for _, mode := range []string{"caller_cancel", "job_cancel", "lease_cancel", "deadline", "config", "credentials", "bundle", "rv", "uid", "term", "lease_id", "same_guard_frame", "replacement", "root_fence", "license"} {
			t.Run(point+"_"+mode, func(t *testing.T) {
				h, j, snapshot := preferredSourceFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				lease, leaseCancel := context.WithCancel(j.leaseCtx)
				defer leaseCancel()
				j.leaseCtx = lease
				var allowed atomic.Bool
				allowed.Store(true)
				var rootAllowed atomic.Bool
				rootAllowed.Store(true)
				h.opts.Allowed = allowed.Load
				h.opts.Fence = func(context.Context, ExecutionGuard) error {
					if !rootAllowed.Load() {
						return ErrFenced
					}
					return nil
				}
				var captured SourceCapture
				change := func() {
					switch mode {
					case "caller_cancel":
						cancel()
					case "job_cancel":
						j.cancel()
					case "lease_cancel":
						leaseCancel()
					case "deadline":
						<-time.After(time.Until(captured.Deadline()) + time.Millisecond)
					case "root_fence":
						rootAllowed.Store(false)
					case "license":
						allowed.Store(false)
					default:
						h.mu.Lock()
						defer h.mu.Unlock()
						switch mode {
						case "config":
							j.guard.ConfigDigest = "new"
						case "credentials":
							j.guard.CredentialDigest = "new"
						case "bundle":
							j.frame.input.BundleID = "new"
						case "rv":
							j.guard.IntegrationResourceVersion = "next"
						case "uid":
							j.guard.Owner.UID = "recreated"
						case "term":
							j.guard.Term.Epoch++
						case "lease_id":
							j.guard.LeaseID = "successor"
						case "same_guard_frame":
							copyInstance := *j.frame.instance
							j.frame.instance = &copyInstance
						case "replacement":
							replacement := *j
							h.jobs[j.key] = &replacement
						}
					}
				}
				if mode == "deadline" {
					h.opts.OperationTimeout = 40 * time.Millisecond
				}
				j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(ctx context.Context) (SourceSnapshot, error) {
					_, g, _ := h.captured(j)
					var err error
					captured, err = h.SourceCapture(ctx, g)
					if err != nil {
						t.Fatal(err)
					}
					if point == "observe" {
						change()
					}
					return snapshot, nil
				}}
				callbacks := 0
				h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
					if point == "observe" {
						t.Fatal("changed acquisition reached commit")
					}
					change()
					// Root rechecks its own external fence before entering WithSource;
					// this seam intentionally does not call root hooks under its lock.
					if err := h.fence(ctx, j, g); err != nil {
						return nil, err
					}
					err := h.WithSource(ctx, captured, func(context.Context, func(context.Context) error) error { callbacks++; return nil })
					return nil, err
				}
				a := h.execute(ctx, j, command{kind: "sync"})
				if a.err == nil || callbacks != 0 {
					t.Fatal("old scope admitted", a, callbacks)
				}
			})
		}
	}
}

func TestWithSourceExcludesReplacementAndFinalDoesNotReenterHostLock(t *testing.T) {
	h, j, _ := preferredSourceFixture(t)
	var retained SourceCapture
	h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
		c, err := h.SourceCapture(ctx, g)
		if err != nil {
			return nil, err
		}
		retained = c
		started, replaced := make(chan struct{}), make(chan struct{})
		err = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
			go func() {
				close(started)
				h.mu.Lock()
				replacement := *j
				h.jobs[j.key] = &replacement
				h.mu.Unlock()
				close(replaced)
			}()
			<-started
			select {
			case <-replaced:
				t.Fatal("replacement escaped exclusion")
			case <-time.After(10 * time.Millisecond):
			}
			// A queued writer makes nested RLock deadlock. The final check must
			// use only pinned lifetime data, not captured()/fence()/SourceCapture.
			checked := make(chan error, 1)
			go func() { checked <- final(ctx) }()
			select {
			case err := <-checked:
				return err
			case <-time.After(time.Second):
				t.Fatal("final reentered host lock")
				return ErrFenced
			}
		})
		<-replaced
		return &Result{Source: &SourceOutcome{ReceiptID: c.RequestID(), Committed: true}}, err
	}
	a := h.execute(context.Background(), j, command{kind: "sync"})
	if !errors.Is(a.err, ErrFenced) || a.result == nil || !a.result.Source.Committed || a.result.Pending != 1 || retained.RequestID() == "" {
		t.Fatal("postcommit replacement lost durable pending", a)
	}
}

func TestWithSourceFinalChecksOriginalCancellationLicenseAndCallbackError(t *testing.T) {
	for _, mode := range []string{"caller", "job", "lease", "license", "deadline", "callback_error"} {
		t.Run(mode, func(t *testing.T) {
			h, j, _ := preferredSourceFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lease, leaseCancel := context.WithCancel(j.leaseCtx)
			defer leaseCancel()
			j.leaseCtx = lease
			var allowed atomic.Bool
			allowed.Store(true)
			h.opts.Allowed = allowed.Load
			if mode == "deadline" {
				h.opts.OperationTimeout = 30 * time.Millisecond
			}
			sentinel := errors.New("callback rolled back")
			var callbackErr error
			h.opts.CommitSource = func(ctx context.Context, g ExecutionGuard, _ SourceSnapshot) (*Result, error) {
				c, err := h.SourceCapture(ctx, g)
				if err != nil {
					return nil, err
				}
				callbackErr = h.WithSource(ctx, c, func(ctx context.Context, final func(context.Context) error) error {
					switch mode {
					case "caller":
						cancel()
					case "job":
						j.cancel()
					case "lease":
						leaseCancel()
					case "license":
						allowed.Store(false)
					case "deadline":
						<-time.After(time.Until(c.Deadline()) + time.Millisecond)
					case "callback_error":
						return sentinel
					}
					return final(ctx)
				})
				return &Result{Applied: 7, Clients: 1, Source: &SourceOutcome{}}, callbackErr
			}
			a := h.execute(ctx, j, command{kind: "sync"})
			if a.err == nil || callbackErr == nil || a.result == nil || a.result.Source.Committed || a.result.Applied != 0 || a.result.Clients != 0 {
				t.Fatal("rollback reported durable success", a, callbackErr)
			}
			if mode == "callback_error" && (callbackErr != sentinel || a.err != sentinel) {
				t.Fatal("callback error changed", callbackErr, a.err)
			}
		})
	}
}

func TestOriginalSourceAllowsIdenticalRefreshAndRejectsRealFrameReplacement(t *testing.T) {
	for _, mode := range []string{"identical", "bundle", "sources"} {
		t.Run(mode, func(t *testing.T) {
			h, j, snapshot := preferredSourceFixture(t)
			input := j.frame.input
			h.opts.Capture = func(context.Context) (Input, error) { return input, nil }
			// Establish the ordinary first complete source digest before polling.
			h.refresh(context.Background())
			original := j.frame.instance
			j.provider = sourceOnlyObserver{Integration: j.provider, observe: func(ctx context.Context) (SourceSnapshot, error) {
				if mode == "bundle" {
					input.BundleID = "replacement-bundle"
				}
				if mode == "sources" {
					input.Sources = nil
				}
				h.refresh(ctx)
				if mode == "identical" && j.frame.instance != original {
					t.Fatal("identical refresh replaced original frame")
				}
				return snapshot, nil
			}}
			err := h.execute(context.Background(), j, command{kind: "sync"}).err
			if mode == "identical" && err != nil {
				t.Fatal("unchanged cut fenced observation", err)
			}
			if mode != "identical" && !errors.Is(err, ErrFenced) {
				t.Fatal("real replacement retained original scope", err)
			}
		})
	}
}
