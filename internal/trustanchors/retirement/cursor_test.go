package retirement

import (
	"context"
	"errors"
	"math"
	"testing"
)

func TestCursorConstructionAndManagedBounds(t *testing.T) {
	for _, history := range []History{{}, {ClusterID: " padded", HistoryID: testHistory().HistoryID}, {ClusterID: "line\nbreak", HistoryID: testHistory().HistoryID}, {ClusterID: "x", HistoryID: "ABCDEF0123456789abcdef0123456789"}, {ClusterID: string([]byte{0xff}), HistoryID: testHistory().HistoryID}} {
		if _, err := New(history, [32]byte{}, 0); !errors.Is(err, ErrInvalid) {
			t.Fatal("noncanonical history accepted")
		}
	}
	if _, err := New(testHistory(), [32]byte{}, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal("retained floor accepted without domain")
	}
	var zero Gate
	if _, err := zero.CaptureWrite(testContext(t), Mark{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("zero gate admitted")
	}
	zero.Close()
	for _, pure := range []uint64{0, 1, math.MaxUint64 - 2, math.MaxUint64 - 1, math.MaxUint64} {
		t.Run(testUintLabel(pure), func(t *testing.T) {
			g := testGate(t)
			m := testMark(pure)
			valid := pure >= 1 && pure < math.MaxUint64-1
			err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
				c, err := s.BeginChange()
				if err != nil {
					return err
				}
				if err := c.Stage(m); err != nil {
					return err
				}
				return c.Resolve(ConfirmedRollback)
			})
			if valid && err != nil {
				t.Fatal("accepted-compatible managed bound refused", err)
			}
			if !valid && (!errors.Is(err, ErrInvalid) || !g.state.Load().closed) {
				t.Fatal("invalid managed bound did not close plan")
			}
		})
	}
	for _, mutate := range []func(*Mark){func(m *Mark) { m.EnvelopeGeneration++ }, func(m *Mark) { m.EnvelopeGeneration = math.MaxUint64 }, func(m *Mark) { m.DomainID = [32]byte{} }, func(m *Mark) { m.PureDigest = [32]byte{} }, func(m *Mark) { m.EnvelopeDigest = [32]byte{} }} {
		m := testMark(1)
		mutate(&m)
		if validMark(m) {
			t.Fatal("malformed managed mark accepted")
		}
	}
}

func testUintLabel(n uint64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n != 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

func TestCursorDormantAndRestoredMaximumFloor(t *testing.T) {
	g := testGate(t)
	if state := g.state.Load(); state.current != nil || state.installed != (Mark{}) {
		t.Fatal("new gate invented accepted/bootstrap authority")
	}
	if _, err := g.CaptureRead(testContext(t), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil snapshot admitted")
	}
	err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error { _, err := s.PrepareBinding(testFrame(testMark(1))); return err })
	if !errors.Is(err, ErrMismatch) || !g.state.Load().closed {
		t.Fatal("raw frame bound without an installed owner")
	}
	m := testMark(math.MaxUint64 - 2)
	g, err = New(testHistory(), m.DomainID, math.MaxUint64)
	if err != nil {
		t.Fatal(err)
	}
	testInstall(t, g, Mark{}, m)
	if g.state.Load().floor != math.MaxUint64 {
		t.Fatal("restored maximum floor wrapped")
	}
	err = writeAt(testContext(t), t, g, m, func(s *WriteScope) error { _, err := s.PrepareBinding(testFrame(m)); return err })
	if !errors.Is(err, ErrRetired) || g.state.Load().current != nil {
		t.Fatal("maximum restored floor allowed a registration")
	}
}

func TestCursorDomainPinnedOnlyOnConfirmedInstall(t *testing.T) {
	g := testGate(t)
	first, second := testMark(1), testMark(2)
	second.DomainID[0] = 3
	if err := writeAt(testContext(t), t, g, Mark{}, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		if err := c.Stage(first); err != nil {
			return err
		}
		return c.Resolve(ConfirmedRollback)
	}); err != nil {
		t.Fatal(err)
	}
	if g.state.Load().domain != ([32]byte{}) {
		t.Fatal("rolled-back proposal sealed domain")
	}
	testInstall(t, g, Mark{}, second)
	err := writeAt(testContext(t), t, g, second, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		return c.Stage(testMark(3))
	})
	if !errors.Is(err, ErrInvalid) || !g.state.Load().closed {
		t.Fatal("confirmed domain was replaced")
	}
	restored, err := New(testHistory(), first.DomainID, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = writeAt(testContext(t), t, restored, Mark{}, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		return c.Stage(second)
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("zero floor forgot restored domain")
	}
}

func TestCursorFullExpectedIdentityAndRegressionRefusal(t *testing.T) {
	mutations := []struct {
		name   string
		change func(*Mark)
	}{
		{"domain", func(m *Mark) { m.DomainID[0]++ }},
		{"envelope-digest", func(m *Mark) { m.EnvelopeDigest[0]++ }},
		{"pure-digest", func(m *Mark) { m.PureDigest[0]++ }},
		{"generation", func(m *Mark) { m.EnvelopeGeneration++; m.PureGeneration++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			g, _ := boundGate(t)
			expected := testMark(1)
			mutation.change(&expected)
			before := g.state.Load()
			err := writeAt(testContext(t), t, g, expected, func(*WriteScope) error { t.Error("mismatched expected owner invoked callback"); return nil })
			if !errors.Is(err, ErrMismatch) || g.state.Load() != before {
				t.Fatal("original expected identity was substituted")
			}
		})
	}
	for _, target := range []Mark{testMark(1), testMark(2)} {
		t.Run("regress-"+testUintLabel(target.PureGeneration), func(t *testing.T) {
			g := testGate(t)
			testInstall(t, g, Mark{}, testMark(2))
			if target.PureGeneration == 2 {
				target.PureDigest[0]++
			}
			err := writeAt(testContext(t), t, g, testMark(2), func(s *WriteScope) error {
				c, err := s.BeginChange()
				if err != nil {
					return err
				}
				return c.Stage(target)
			})
			if !errors.Is(err, ErrMismatch) || !g.state.Load().closed {
				t.Fatal("lower/changed same-generation mark accepted")
			}
		})
	}
}

func TestCursorSkippedGenerationRetiresExactSnapshot(t *testing.T) {
	g, original := boundGate(t)
	oldAttempt, err := g.CaptureRead(testContext(t), original)
	if err != nil {
		t.Fatal(err)
	}
	testInstall(t, g, testMark(1), testMark(9))
	if g.state.Load().floor != 9 || g.state.Load().current != nil {
		t.Fatal("install did not retire below skipped owner")
	}
	if err := g.WithRead(oldAttempt, func(ReadScope) error { t.Error("old snapshot used after install"); return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	// The same effective synthetic OwnerID still receives a different Snapshot.
	newSnapshot := testBind(t, g, testFrame(testMark(9)))
	if newSnapshot == original {
		t.Fatal("higher version reused old pointer")
	}
	if err := readAt(testContext(t), t, g, original, func(ReadScope) error { return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal("old pointer aliased higher version")
	}
	if err := readAt(testContext(t), t, g, newSnapshot, func(s ReadScope) error { return s.Check() }); err != nil {
		t.Fatal(err)
	}
	foreign, _ := boundGate(t)
	if _, err := foreign.CaptureRead(testContext(t), newSnapshot); !errors.Is(err, ErrInvalid) {
		t.Fatal("foreign gate accepted registration")
	}
}

func TestCursorExactNoOpAndFrameReuse(t *testing.T) {
	g, original := boundGate(t)
	testInstall(t, g, testMark(1), testMark(1))
	if g.state.Load().current != original {
		t.Fatal("exact installed no-op changed registration")
	}
	if snapshot := testBind(t, g, testFrame(testMark(1))); snapshot != original {
		t.Fatal("equal frame publication replaced pointer")
	}
	frame := testFrame(testMark(1))
	frame.OwnerID[0]++
	err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error { _, err := s.PrepareBinding(frame); return err })
	if !errors.Is(err, ErrMismatch) || !g.state.Load().closed {
		t.Fatal("same owner version accepted changed full projection identity")
	}
}

func TestCursorPendingBindingAndConfirmedNoPublication(t *testing.T) {
	g := testGate(t)
	m := testMark(1)
	testInstall(t, g, Mark{}, m)
	var pending *Snapshot
	err := writeAt(testContext(t), t, g, m, func(s *WriteScope) error {
		b, err := s.PrepareBinding(testFrame(m))
		if err != nil {
			return err
		}
		pending = b.Snapshot()
		if pending == nil || g.state.Load().current != nil {
			t.Error("prepared binding was not inactive")
		}
		return b.Resolve(ConfirmedNotPublished)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := readAt(testContext(t), t, g, pending, func(ReadScope) error { t.Error("inactive snapshot entered"); return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	snapshot := testBind(t, g, testFrame(m))
	if snapshot == pending {
		t.Fatal("discarded binding pointer reused")
	}
}

func TestCursorBindingReplayAndPublicationClassification(t *testing.T) {
	for _, mode := range []string{"apply-twice", "after-resolve", "not-published-after-apply", "published-without-apply", "unknown", "resolve-twice"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
				b, err := s.PrepareBinding(testFrame(testMark(1)))
				if err != nil {
					return err
				}
				switch mode {
				case "published-without-apply":
					return b.Resolve(ConfirmedPublished)
				case "unknown":
					return b.Resolve(PublishUnknown)
				case "resolve-twice":
					if err := b.Resolve(ConfirmedNotPublished); err != nil {
						return err
					}
					return b.Resolve(ConfirmedNotPublished)
				}
				if err := b.Apply(); err != nil {
					return err
				}
				if mode == "after-resolve" {
					if err := b.Resolve(ConfirmedPublished); err != nil {
						return err
					}
				}
				if mode == "not-published-after-apply" {
					return b.Resolve(ConfirmedNotPublished)
				}
				return b.Apply()
			})
			if err == nil || !g.state.Load().closed || g.state.Load().current != nil {
				t.Fatal("invalid publication plan left admission open")
			}
			var effectErr *EffectError
			if !errors.As(err, &effectErr) {
				t.Fatal("invalid/uncertain plan lost effect classification")
			}
			if mode == "after-resolve" && effectErr.Effect() != PublishedEffect {
				t.Fatal("replay erased confirmed publication")
			}
			if (mode == "apply-twice" || mode == "not-published-after-apply") && effectErr.Effect() != PublicationAppliedEffect {
				t.Fatal("unconfirmed swap counted as no effect")
			}
		})
	}
}

func TestCursorBindingApplyAndScopeCloseShareCAS(t *testing.T) {
	for range 32 {
		g, _ := boundGate(t)
		done := make(chan error, 1)
		var escaped *Binding
		_ = writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
			b, err := s.PrepareBinding(testFrame(testMark(1)))
			if err != nil {
				return err
			}
			escaped = b
			go func() { done <- b.Apply() }()
			return nil
		})
		err := receive(t, done)
		if err != nil && !errors.Is(err, ErrScope) {
			t.Fatal(err)
		}
		before := g.state.Load()
		if !before.closed || before.current != nil || before.writer != nil {
			t.Fatal("unresolved publication escaped cleanup")
		}
		if err := escaped.Apply(); !errors.Is(err, ErrScope) {
			t.Fatal("escaped Apply accepted after exit")
		}
		if g.state.Load() != before {
			t.Fatal("escaped Apply mutated after scope close")
		}
	}
}

func TestCursorWithdrawalAndMonotoneFloor(t *testing.T) {
	g, old := boundGate(t)
	if err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
		w, err := s.PrepareWithdrawal()
		if err != nil {
			return err
		}
		return w.Resolve(ConfirmedDurableDeny)
	}); err != nil {
		t.Fatal(err)
	}
	if g.state.Load().floor != 2 || g.state.Load().current != nil {
		t.Fatal("denial was not inclusive")
	}
	testInstall(t, g, testMark(1), testMark(1))
	if g.state.Load().floor != 2 {
		t.Fatal("same-version no-op reduced denial")
	}
	if err := readAt(testContext(t), t, g, old, func(ReadScope) error { return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal("denied snapshot remained usable")
	}
	testInstall(t, g, testMark(1), testMark(4))
	newSnapshot := testBind(t, g, testFrame(testMark(4)))
	if newSnapshot == old || g.state.Load().floor != 4 {
		t.Fatal("later install lost floor or reused denied pointer")
	}
	if err := readAt(testContext(t), t, g, old, func(ReadScope) error { return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal("denied old snapshot aliased new registration")
	}
	if err := writeAt(testContext(t), t, g, testMark(4), func(s *WriteScope) error { _, err := s.PrepareBinding(testFrame(testMark(1))); return err }); !errors.Is(err, ErrMismatch) {
		t.Fatal("old exact mark rebound")
	}
	// A separate gate shows same-version rebinding is refused by the floor itself.
	g, _ = boundGate(t)
	if err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
		w, err := s.PrepareWithdrawal()
		if err != nil {
			return err
		}
		return w.Resolve(ConfirmedDurableDeny)
	}); err != nil {
		t.Fatal(err)
	}
	err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error { _, err := s.PrepareBinding(testFrame(testMark(1))); return err })
	if !errors.Is(err, ErrRetired) {
		t.Fatal("inclusive floor allowed same-version binding")
	}
}

func TestCursorUnknownAndFailedDenyClose(t *testing.T) {
	for _, mode := range []string{"unknown-install", "installed-without-target", "failed-deny", "unknown-publication"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			_ = writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
				switch mode {
				case "failed-deny":
					w, err := s.PrepareWithdrawal()
					if err != nil {
						return err
					}
					return w.Resolve(FailedOrUnknown)
				case "unknown-publication":
					b, err := s.PrepareBinding(testFrame(testMark(1)))
					if err != nil {
						return err
					}
					return b.Resolve(PublishUnknown)
				default:
					c, err := s.BeginChange()
					if err != nil {
						return err
					}
					if mode == "unknown-install" {
						return c.Resolve(ChangeUnknown)
					}
					return c.Resolve(ConfirmedInstalled)
				}
			})
			if !g.state.Load().closed || g.state.Load().current != nil {
				t.Fatal("uncertain effect restored old admission")
			}
		})
	}
}

func TestCursorClosedGateCompletionCannotReopen(t *testing.T) {
	for _, mode := range []string{"install", "deny", "publish"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
				switch mode {
				case "install":
					c, err := s.BeginChange()
					if err != nil {
						return err
					}
					if err := c.Stage(testMark(3)); err != nil {
						return err
					}
					g.Close()
					return c.Resolve(ConfirmedInstalled)
				case "deny":
					w, err := s.PrepareWithdrawal()
					if err != nil {
						return err
					}
					g.Close()
					return w.Resolve(ConfirmedDurableDeny)
				default:
					b, err := s.PrepareBinding(testFrame(testMark(1)))
					if err != nil {
						return err
					}
					if err := b.Apply(); err != nil {
						return err
					}
					g.Close()
					return b.Resolve(ConfirmedPublished)
				}
			})
			var effectErr *EffectError
			if !errors.As(err, &effectErr) || !errors.Is(err, ErrClosed) {
				t.Fatal("terminal close lost original completed-effect classification")
			}
			if !g.state.Load().closed || g.state.Load().current != nil {
				t.Fatal("confirmed effect reopened explicit close")
			}
		})
	}
}

func TestCursorOriginalCancellationRefusesFreshActions(t *testing.T) {
	for _, mode := range []string{"stage", "apply", "withdraw-check"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			err := writeAt(ctx, t, g, testMark(1), func(s *WriteScope) error {
				switch mode {
				case "stage":
					c, err := s.BeginChange()
					if err != nil {
						return err
					}
					cancel()
					err = c.Stage(testMark(3))
					if !errors.Is(err, errOriginalDone) {
						t.Error("post-cancel target accepted")
					}
					if rollbackErr := c.Resolve(ConfirmedRollback); rollbackErr != nil {
						return rollbackErr
					}
					return err
				case "apply":
					b, err := s.PrepareBinding(testFrame(testMark(1)))
					if err != nil {
						return err
					}
					cancel()
					err = b.Apply()
					if !errors.Is(err, errOriginalDone) {
						t.Error("post-cancel swap accepted")
					}
					if resolveErr := b.Resolve(ConfirmedNotPublished); resolveErr != nil {
						return resolveErr
					}
					return err
				default:
					w, err := s.PrepareWithdrawal()
					if err != nil {
						return err
					}
					cancel()
					return w.CheckPrepared()
				}
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatal("original cancellation category not preserved")
			}
		})
	}
}

func TestCursorCompletedEffectCannotBecomeRollback(t *testing.T) {
	for _, mode := range []string{"install", "deny", "publish"} {
		t.Run(mode, func(t *testing.T) {
			g, _ := boundGate(t)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			expected := NoEffect
			err := writeAt(ctx, t, g, testMark(1), func(s *WriteScope) error {
				switch mode {
				case "install":
					c, err := s.BeginChange()
					if err != nil {
						return err
					}
					if err := c.Stage(testMark(3)); err != nil {
						return err
					}
					if err := c.Resolve(ConfirmedInstalled); err != nil {
						return err
					}
					expected = InstalledEffect
					cancel()
					return c.Resolve(ConfirmedRollback)
				case "deny":
					w, err := s.PrepareWithdrawal()
					if err != nil {
						return err
					}
					if err := w.Resolve(ConfirmedDurableDeny); err != nil {
						return err
					}
					expected = DeniedEffect
					cancel()
					return w.Resolve(FailedOrUnknown)
				default:
					b, err := s.PrepareBinding(testFrame(testMark(1)))
					if err != nil {
						return err
					}
					if err := b.Apply(); err != nil {
						return err
					}
					if err := b.Resolve(ConfirmedPublished); err != nil {
						return err
					}
					expected = PublishedEffect
					cancel()
					return b.Resolve(ConfirmedNotPublished)
				}
			})
			var effectErr *EffectError
			if !errors.As(err, &effectErr) || effectErr.Effect() != expected || !errors.Is(err, ErrUsed) {
				t.Fatal("duplicate resolution rewrote completed effect")
			}
			state := g.state.Load()
			if !state.closed || state.current != nil {
				t.Fatal("invalid duplicate plan reopened")
			}
			if mode == "install" && (state.installed != testMark(3) || state.floor != 3) {
				t.Fatal("duplicate resolution rolled back installation")
			}
			if mode == "deny" && state.floor != 2 {
				t.Fatal("duplicate resolution rolled back denial")
			}
		})
	}
}

func TestCursorEscapedBindingAndWithdrawalCannotMutateSuccessor(t *testing.T) {
	g, _ := boundGate(t)
	var binding *Binding
	if err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
		b, err := s.PrepareBinding(testFrame(testMark(1)))
		if err != nil {
			return err
		}
		binding = b
		if err := b.Apply(); err != nil {
			return err
		}
		return b.Resolve(ConfirmedPublished)
	}); err != nil {
		t.Fatal(err)
	}
	var withdrawal *Withdrawal
	if err := writeAt(testContext(t), t, g, testMark(1), func(s *WriteScope) error {
		before := g.state.Load()
		if err := binding.Apply(); !errors.Is(err, ErrScope) {
			t.Error("escaped binding Apply accepted successor")
		}
		if err := binding.Resolve(ConfirmedNotPublished); !errors.Is(err, ErrScope) {
			t.Error("escaped binding completion accepted successor")
		}
		if g.state.Load() != before {
			t.Error("escaped binding mutated successor")
		}
		w, err := s.PrepareWithdrawal()
		if err != nil {
			return err
		}
		withdrawal = w
		return w.Resolve(ConfirmedDurableDeny)
	}); err != nil {
		t.Fatal(err)
	}
	testInstall(t, g, testMark(1), testMark(4))
	if err := writeAt(testContext(t), t, g, testMark(4), func(s *WriteScope) error {
		before := g.state.Load()
		if err := withdrawal.Resolve(ConfirmedDurableDeny); !errors.Is(err, ErrScope) {
			t.Error("escaped withdrawal accepted successor")
		}
		if g.state.Load() != before {
			t.Error("escaped withdrawal mutated successor floor")
		}
		return s.Check()
	}); err != nil {
		t.Fatal(err)
	}
}
