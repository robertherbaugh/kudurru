package retirement

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestScopesOneShotCopiesAndRefusals(t *testing.T) {
	g, snapshot := boundGate(t)
	a, _ := g.CaptureRead(testContext(t), snapshot)
	copyAttempt := *a
	start, done := make(chan struct{}), make(chan error, 2)
	var called atomic.Int32
	for _, attempt := range []*ReadAttempt{a, &copyAttempt} {
		go func() { <-start; done <- g.WithRead(attempt, func(ReadScope) error { called.Add(1); return nil }) }()
	}
	close(start)
	err1, err2 := receive(t, done), receive(t, done)
	if (err1 != nil || !errors.Is(err2, ErrUsed)) && (err2 != nil || !errors.Is(err1, ErrUsed)) || called.Load() != 1 {
		t.Fatal("read copies did not share one-shot consumption")
	}
	w, _ := g.CaptureWrite(testContext(t), Mark{})
	wcopy := *w
	if err := g.WithWrite(w, func(*WriteScope) error { t.Error("stale expected callback invoked"); return nil }); !errors.Is(err, ErrMismatch) {
		t.Fatal(err)
	}
	if err := g.WithWrite(&wcopy, func(*WriteScope) error { return nil }); !errors.Is(err, ErrUsed) {
		t.Fatal("mismatch reset original attempt")
	}
	bad, _ := g.CaptureWrite(testContext(t), testMark(1))
	if err := g.WithWrite(bad, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := g.WithWrite(bad, func(*WriteScope) error { return nil }); !errors.Is(err, ErrUsed) {
		t.Fatal("nil callback reset consumed attempt")
	}
	winner, _ := g.CaptureWrite(testContext(t), testMark(1))
	winnerCopy := *winner
	startWrites, writeDone := make(chan struct{}), make(chan error, 2)
	called.Store(0)
	for _, attempt := range []*WriteAttempt{winner, &winnerCopy} {
		go func() {
			<-startWrites
			writeDone <- g.WithWrite(attempt, func(*WriteScope) error { called.Add(1); return nil })
		}()
	}
	close(startWrites)
	err1, err2 = receive(t, writeDone), receive(t, writeDone)
	if (err1 != nil || !errors.Is(err2, ErrUsed)) && (err2 != nil || !errors.Is(err1, ErrUsed)) || called.Load() != 1 {
		t.Fatal("write copies did not share one-shot consumption")
	}
}

func TestScopesForeignAttemptsAndInactiveScopes(t *testing.T) {
	g, snapshot := boundGate(t)
	foreign := testGate(t)
	r, _ := g.CaptureRead(testContext(t), snapshot)
	w, _ := g.CaptureWrite(testContext(t), testMark(1))
	if err := foreign.WithRead(r, func(ReadScope) error { t.Error("foreign read entered"); return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := foreign.WithWrite(w, func(*WriteScope) error { t.Error("foreign write entered"); return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := g.WithRead(r, func(s ReadScope) error { return s.Check() }); err != nil {
		t.Fatal("foreign gate consumed original read", err)
	}
	if err := g.WithWrite(w, func(s *WriteScope) error { return s.Check() }); err != nil {
		t.Fatal("foreign gate consumed original write", err)
	}
	if err := (ReadScope{}).Check(); !errors.Is(err, ErrScope) {
		t.Fatal("zero read scope admitted")
	}
	if err := (*WriteScope)(nil).Check(); !errors.Is(err, ErrScope) {
		t.Fatal("nil write scope admitted")
	}
}

func TestScopesOriginalFiniteLifetime(t *testing.T) {
	g := testGate(t)
	if _, err := g.CaptureWrite(context.Background(), Mark{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded lifetime admitted")
	}
	if _, err := g.CaptureWrite(nil, Mark{}); !errors.Is(err, ErrInvalid) { //nolint:staticcheck // Intentionally verify that a nil original context refuses admission.
		t.Fatal("nil context admitted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	a, err := g.CaptureWrite(ctx, Mark{})
	if err != nil {
		t.Fatal(err)
	}
	if a.cell.life.original != ctx || a.cell.life.until.Sub(a.cell.life.at) != MaxLifetime {
		t.Fatal("original lifetime replaced or cap renewed")
	}
	deadline, _ := ctx.Deadline()
	if !a.cell.life.deadline.Equal(deadline) {
		t.Fatal("original deadline replaced")
	}
	cancel()
	if err := g.WithWrite(a, func(*WriteScope) error { t.Error("canceled captured attempt invoked"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := g.WithWrite(a, func(*WriteScope) error { return nil }); !errors.Is(err, ErrUsed) {
		t.Fatal("canceled refusal was reusable")
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := g.CaptureWrite(expired, Mark{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("already-expired original accepted")
	}
}

// This custom original context records ALL interface calls. Its cached Done can
// report either original category without substituting another context. Calls
// from a final check panic; outward Err classification verifies queue release.
type observedContext struct {
	base           context.Context
	done           chan struct{}
	category       error
	gate           *Gate
	forbid         atomic.Bool
	expectReleased atomic.Bool
	calls          atomic.Int32
}

func (c *observedContext) touch() {
	c.calls.Add(1)
	if c.forbid.Load() {
		panic("context-interface method used by memory-only path")
	}
	if !c.gate.mu.TryLock() {
		panic("context-interface method used under queue lock")
	}
	bad := c.expectReleased.Load() && c.gate.writer
	c.gate.mu.Unlock()
	if bad {
		panic("original Err called before counted writer release")
	}
}

func (c *observedContext) Deadline() (time.Time, bool) { c.touch(); return c.base.Deadline() }
func (c *observedContext) Done() <-chan struct{}       { c.touch(); return c.done }
func (c *observedContext) Value(key any) any           { c.touch(); return c.base.Value(key) }
func (c *observedContext) Err() error {
	c.touch()
	select {
	case <-c.done:
		return c.category
	default:
		return nil
	}
}

func TestScopesCachedContextAndUnlockedClassification(t *testing.T) {
	for _, category := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(category.Error(), func(t *testing.T) {
			g := testGate(t)
			ctx := &observedContext{base: testContext(t), done: make(chan struct{}), category: category, gate: g}
			a, err := g.CaptureWrite(ctx, Mark{})
			if err != nil {
				t.Fatal(err)
			}
			err = g.WithWrite(a, func(s *WriteScope) error {
				c, err := s.BeginChange()
				if err != nil {
					return err
				}
				if err := c.Stage(testMark(1)); err != nil {
					return err
				}
				before := ctx.calls.Load()
				ctx.forbid.Store(true)
				defer ctx.forbid.Store(false)
				if err := c.CheckPrepared(); err != nil {
					return err
				}
				close(ctx.done)
				err = s.Check()
				if !errors.Is(err, errOriginalDone) {
					t.Error("cached Done not observed")
				}
				if ctx.calls.Load() != before {
					t.Error("final predicate called original context interface")
				}
				ctx.forbid.Store(false)
				ctx.expectReleased.Store(true)
				if resolveErr := c.Resolve(ConfirmedRollback); resolveErr != nil {
					return resolveErr
				}
				return err
			})
			if !errors.Is(err, category) {
				t.Fatal("original context category changed")
			}
			if g.state.Load().closed {
				t.Fatal("confirmed rollback after cancellation closed unchanged cursor")
			}
		})
	}
}

func TestScopesFinalApplyUsesOnlyCachedContext(t *testing.T) {
	g, _ := boundGate(t)
	ctx := &observedContext{base: testContext(t), done: make(chan struct{}), category: context.Canceled, gate: g}
	a, err := g.CaptureWrite(ctx, testMark(1))
	if err != nil {
		t.Fatal(err)
	}
	err = g.WithWrite(a, func(s *WriteScope) error {
		b, err := s.PrepareBinding(testFrame(testMark(1)))
		if err != nil {
			return err
		}
		before := ctx.calls.Load()
		ctx.forbid.Store(true)
		err = b.Apply()
		ctx.forbid.Store(false)
		if err != nil {
			return err
		}
		if before != ctx.calls.Load() {
			t.Error("Apply invoked context interface")
		}
		close(ctx.done)
		return b.Resolve(ConfirmedPublished)
	})
	if err != nil {
		t.Fatal("completed publication changed to cancellation", err)
	}
}

func TestScopesErrorPanicAndEscapedLifetime(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "panic"}[panics], func(t *testing.T) {
			g := testGate(t)
			var escaped *WriteScope
			cause := errors.New("synthetic callback refusal")
			func() {
				defer func() {
					p := recover()
					if panics && p != cause {
						t.Error("original panic not rethrown")
					}
					if !panics && p != nil {
						panic(p)
					}
				}()
				err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
					escaped = s
					if panics {
						panic(cause)
					}
					return cause
				})
				if !errors.Is(err, cause) {
					t.Error("callback refusal changed")
				}
			}()
			if !errors.Is(escaped.Check(), ErrScope) {
				t.Fatal("escaped scope stayed active")
			}
			g.mu.Lock()
			balanced := !g.writer && g.readers == 0
			g.mu.Unlock()
			if !balanced || g.state.Load().writer != nil {
				t.Fatal("panic/error did not release exactly once")
			}
			if err := writeAt(testContext(t), t, g, Mark{}, func(*WriteScope) error { return nil }); err != nil {
				t.Fatal("successor blocked after no-plan refusal", err)
			}
		})
	}
	g, snapshot := boundGate(t)
	var escaped ReadScope
	if err := readAt(testContext(t), t, g, snapshot, func(s ReadScope) error { escaped = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(escaped.Check(), ErrScope) {
		t.Fatal("escaped read scope remained active")
	}
}

func TestScopesPostEffectCancellationAndCallbackErrors(t *testing.T) {
	for _, kind := range []string{"install", "deny", "publish", "applied"} {
		for _, refuses := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "-nil", true: "-error"}[refuses], func(t *testing.T) {
				g, _ := boundGate(t)
				ctx, cancel := context.WithCancel(testContext(t))
				defer cancel()
				cause := errors.New("synthetic post-effect callback refusal")
				expectedEffect := NoEffect
				err := writeAt(ctx, t, g, testMark(1), func(s *WriteScope) error {
					switch kind {
					case "install":
						c, err := s.BeginChange()
						if err != nil {
							return err
						}
						if err := c.Stage(testMark(3)); err != nil {
							return err
						}
						cancel()
						if err := c.Resolve(ConfirmedInstalled); err != nil {
							return err
						}
						expectedEffect = InstalledEffect
					case "deny":
						w, err := s.PrepareWithdrawal()
						if err != nil {
							return err
						}
						if err := w.CheckPrepared(); err != nil {
							return err
						}
						cancel()
						if err := w.Resolve(ConfirmedDurableDeny); err != nil {
							return err
						}
						expectedEffect = DeniedEffect
					default:
						b, err := s.PrepareBinding(testFrame(testMark(1)))
						if err != nil {
							return err
						}
						if err := b.Apply(); err != nil {
							return err
						}
						cancel()
						if kind == "publish" {
							if err := b.Resolve(ConfirmedPublished); err != nil {
								return err
							}
							expectedEffect = PublishedEffect
						} else {
							expectedEffect = PublicationAppliedEffect
						}
					}
					if refuses {
						return cause
					}
					return nil
				})
				if kind != "applied" && !refuses {
					if err != nil {
						t.Fatal("confirmed original effect relabeled cancellation", err)
					}
				} else {
					var effectErr *EffectError
					if !errors.As(err, &effectErr) || effectErr.Effect() != expectedEffect {
						t.Fatal("original completed/uncertain effect lost")
					}
					if refuses && !errors.Is(err, cause) {
						t.Fatal("callback refusal category lost")
					}
				}
				state := g.state.Load()
				if kind == "install" && (state.installed != testMark(3) || state.floor != 3 || state.current != nil) {
					t.Fatal("confirmed installed effect was rolled back")
				}
				if kind == "deny" && (state.floor != 2 || state.current != nil) {
					t.Fatal("confirmed denied effect was rolled back")
				}
				if kind == "publish" && (state.current == nil || state.closed) {
					t.Fatal("confirmed publication lost")
				}
				if kind == "applied" && (!state.closed || state.current != nil) {
					t.Fatal("unresolved applied publication reopened")
				}
			})
		}
	}
}

func TestScopesUnresolvedAndSecondPlanCloseAdmission(t *testing.T) {
	for _, mode := range []string{"change", "binding", "withdrawal", "second-plan", "panic"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			func() {
				defer func() {
					if p := recover(); p != nil && mode != "panic" {
						panic(p)
					}
				}()
				_ = writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
					switch mode {
					case "binding":
						_, err := s.PrepareBinding(testFrame(testMark(1)))
						return err
					case "withdrawal":
						_, err := s.PrepareWithdrawal()
						return err
					default:
						c, err := s.BeginChange()
						if err != nil {
							return err
						}
						if mode == "panic" {
							panic("synthetic unresolved effect")
						}
						if mode == "second-plan" {
							if err := c.Resolve(ConfirmedRollback); err != nil {
								return err
							}
							_, err := s.BeginChange()
							return err
						}
						return nil
					}
				})
			}()
			if !g.state.Load().closed || g.state.Load().current != nil {
				t.Fatal("unresolved/invalid plan admitted old registration")
			}
		})
	}
}

func TestScopesEscapedPlansCannotMutateSuccessor(t *testing.T) {
	g := testGate(t)
	var escaped *Change
	var stale *gateState
	if err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		escaped = c
		stale = g.state.Load()
		return c.Resolve(ConfirmedRollback)
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
		before := g.state.Load()
		if err := escaped.Stage(testMark(1)); !errors.Is(err, ErrScope) {
			t.Error("escaped stage accepted successor")
		}
		if err := escaped.Resolve(ConfirmedInstalled); !errors.Is(err, ErrScope) {
			t.Error("escaped completion accepted successor")
		}
		if g.state.Load() != before {
			t.Error("escaped operation mutated successor cursor")
		}
		proposal := *stale
		proposal.installed = testMark(1)
		if g.state.CompareAndSwap(stale, &proposal) {
			t.Error("stale pre-close CAS adopted successor")
		}
		return s.Check()
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScopesConcurrentResolutionVersusExit(t *testing.T) {
	for range 32 {
		g := testGate(t)
		done := make(chan error, 1)
		var escaped *Change
		_ = writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
			c, err := s.BeginChange()
			if err != nil {
				return err
			}
			if err := c.Stage(testMark(1)); err != nil {
				return err
			}
			escaped = c
			go func() { done <- c.Resolve(ConfirmedInstalled) }()
			return nil
		})
		err := receive(t, done)
		if err != nil && !errors.Is(err, ErrScope) {
			t.Fatal("unexpected completion/exit race result", err)
		}
		state := g.state.Load()
		if state.writer != nil || (!state.closed && state.installed != testMark(1)) {
			t.Fatal("scope exit split from cursor mutation")
		}
		if state.closed && state.current != nil {
			t.Fatal("unknown outcome left usable registration")
		}
		before := g.state.Load()
		if err := escaped.Resolve(ConfirmedRollback); !errors.Is(err, ErrScope) {
			t.Fatal("post-exit completion not refused")
		}
		if g.state.Load() != before {
			t.Fatal("escaped completion mutated after exit")
		}
	}
}
