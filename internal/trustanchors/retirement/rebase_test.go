package retirement

import (
	"errors"
	"math"
	"testing"
)

func testRebase(t *testing.T, g *Gate, expected, next Mark, retiredThrough uint64, outcome ChangeOutcome) error {
	t.Helper()
	return writeAt(testContext(t), t, g, expected, func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		if err := c.StageRebase(next, retiredThrough); err != nil {
			return err
		}
		if err := c.CheckPrepared(); err != nil {
			return err
		}
		return c.Resolve(outcome)
	})
}

// A sibling is another owner of the same domain at the same generation, as two
// writers of one history produce after a promotion.
func testSibling(pure uint64) Mark {
	m := testMark(pure)
	m.EnvelopeDigest[31], m.PureDigest[31] = 7, 7
	return m
}

func TestCursorRebaseAdoptsLowerAndEqualGenerationAndRetiresRegistration(t *testing.T) {
	for _, target := range []Mark{testMark(3), testSibling(5), testMark(8)} {
		t.Run(testUintLabel(target.PureGeneration), func(t *testing.T) {
			g := testGate(t)
			testInstall(t, g, Mark{}, testMark(5))
			old := testBind(t, g, testFrame(testMark(5)))
			held, err := g.CaptureRead(testContext(t), old)
			if err != nil {
				t.Fatal(err)
			}
			stale, err := g.CaptureWrite(testContext(t), testMark(5))
			if err != nil {
				t.Fatal(err)
			}
			if g.state.Load().floor != 5 {
				t.Fatal("fixture floor")
			}
			if err := testRebase(t, g, testMark(5), target, target.EnvelopeGeneration-1, ConfirmedInstalled); err != nil {
				t.Fatal(err)
			}
			state := g.state.Load()
			if state.closed || state.installed != target || state.floor != target.EnvelopeGeneration-1 || state.current != nil || !g.MatchesInstalled(target) || g.MatchesInstalled(testMark(5)) {
				t.Fatal("rebase did not install the adopted owner on its own floor")
			}
			if err := g.WithRead(held, func(ReadScope) error { t.Error("replaced owner's registration read after rebase"); return nil }); !errors.Is(err, ErrRetired) {
				t.Fatal(err)
			}
			if err := g.WithWrite(stale, func(*WriteScope) error { t.Error("write captured for the replaced owner ran"); return nil }); !errors.Is(err, ErrMismatch) {
				t.Fatal(err)
			}
			fresh := testBind(t, g, testFrame(target))
			if fresh == old {
				t.Fatal("adopted owner reused the replaced registration")
			}
			if err := readAt(testContext(t), t, g, fresh, func(s ReadScope) error { return s.Check() }); err != nil {
				t.Fatal(err)
			}
			if err := readAt(testContext(t), t, g, old, func(ReadScope) error { return nil }); !errors.Is(err, ErrRetired) {
				t.Fatal("replaced registration aliased the adopted owner")
			}
			// The adopted branch continues under the ordinary monotone rule.
			next := testMark(target.PureGeneration + 1)
			testInstall(t, g, target, next)
			if g.state.Load().floor != target.EnvelopeGeneration {
				t.Fatal("successor after rebase lost the monotone floor")
			}
			err = writeAt(testContext(t), t, g, next, func(s *WriteScope) error {
				c, err := s.BeginChange()
				if err != nil {
					return err
				}
				return c.Stage(target)
			})
			if !errors.Is(err, ErrMismatch) || !g.state.Load().closed {
				t.Fatal("ordinary stage regressed after a rebase")
			}
		})
	}
}

func TestCursorRebaseCanKeepTheAdoptedOwnerRetired(t *testing.T) {
	g, old := boundGate(t)
	testInstall(t, g, testMark(1), testMark(6))
	target := testMark(2)
	if err := testRebase(t, g, testMark(6), target, target.EnvelopeGeneration, ConfirmedInstalled); err != nil {
		t.Fatal(err)
	}
	state := g.state.Load()
	if state.installed != target || state.floor != target.EnvelopeGeneration || g.MatchesInstalled(target) {
		t.Fatal("inclusive rebased floor was not retained")
	}
	err := writeAt(testContext(t), t, g, target, func(s *WriteScope) error { _, err := s.PrepareBinding(testFrame(target)); return err })
	if !errors.Is(err, ErrRetired) {
		t.Fatal("retired adopted owner admitted a binding", err)
	}
	if err := readAt(testContext(t), t, g, old, func(ReadScope) error { return nil }); err == nil {
		t.Fatal("old registration read through a retired rebase")
	}
	// A terminal floor is also a valid rebased floor and denies forever.
	g = testGate(t)
	testInstall(t, g, Mark{}, testMark(6))
	if err := testRebase(t, g, testMark(6), target, math.MaxUint64, ConfirmedInstalled); err != nil {
		t.Fatal(err)
	}
	if g.state.Load().floor != math.MaxUint64 || g.MatchesInstalled(target) {
		t.Fatal("terminal rebased floor reopened")
	}
}

func TestCursorRebaseRollbackAndUnknownKeepTheInstalledOwner(t *testing.T) {
	g, old := boundGate(t)
	testInstall(t, g, testMark(1), testMark(4))
	current := testBind(t, g, testFrame(testMark(4)))
	before := g.state.Load()
	if err := testRebase(t, g, testMark(4), testMark(2), 2, ConfirmedRollback); err != nil {
		t.Fatal(err)
	}
	after := g.state.Load()
	if after.installed != before.installed || after.floor != before.floor || after.current != current || after.closed {
		t.Fatal("rolled-back rebase changed the installed owner")
	}
	if err := readAt(testContext(t), t, g, current, func(s ReadScope) error { return s.Check() }); err != nil {
		t.Fatal(err)
	}
	if err := readAt(testContext(t), t, g, old, func(ReadScope) error { return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal("rolled-back rebase reopened an earlier registration")
	}
	if err := testRebase(t, g, testMark(4), testMark(2), 2, ChangeUnknown); !errors.Is(err, ErrClosed) || !g.state.Load().closed {
		t.Fatal("unknown rebase completion left admission open", err)
	}
}

func TestCursorRebaseRefusesForeignDomainUnboundGateAndShallowFloor(t *testing.T) {
	foreign := testMark(2)
	foreign.DomainID[0]++
	cases := []struct {
		name    string
		install bool
		target  Mark
		floor   uint64
		want    error
	}{
		{"foreign-domain", true, foreign, foreign.EnvelopeGeneration - 1, ErrInvalid},
		{"malformed-mark", true, Mark{}, 0, ErrInvalid},
		{"floor-below-adopted-predecessor", true, testMark(3), testMark(3).EnvelopeGeneration - 2, ErrInvalid},
		{"same-mark", true, testMark(5), testMark(5).EnvelopeGeneration - 1, ErrMismatch},
		{"never-installed", false, testMark(3), testMark(3).EnvelopeGeneration - 1, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := testGate(t)
			expected := Mark{}
			if tc.install {
				expected = testMark(5)
				testInstall(t, g, Mark{}, expected)
			}
			err := testRebase(t, g, expected, tc.target, tc.floor, ConfirmedInstalled)
			if !errors.Is(err, tc.want) || !g.state.Load().closed {
				t.Fatal("invalid rebase did not close its plan", err)
			}
			if state := g.state.Load(); state.installed != expected {
				t.Fatal("refused rebase replaced the installed owner")
			}
		})
	}
	// One plan stages once, by either lane.
	g := testGate(t)
	testInstall(t, g, Mark{}, testMark(5))
	err := writeAt(testContext(t), t, g, testMark(5), func(s *WriteScope) error {
		c, err := s.BeginChange()
		if err != nil {
			return err
		}
		if err := c.StageRebase(testMark(3), 3); err != nil {
			return err
		}
		return c.Stage(testMark(9))
	})
	if !errors.Is(err, ErrUsed) || !g.state.Load().closed {
		t.Fatal("a rebased plan staged a second owner", err)
	}
}

// The adopted owner can be one this gate installed before: an old primary that
// advanced once returns to the owner its promoted follower still holds. A write
// attempt captured for that owner before the split finds its exact expected
// mark installed again, and must still not run.
func TestCursorRebaseToAnEarlierOwnerRefusesItsOldAttempts(t *testing.T) {
	g := testGate(t)
	testInstall(t, g, Mark{}, testMark(5))
	earlier := testBind(t, g, testFrame(testMark(5)))
	stale, err := g.CaptureWrite(testContext(t), testMark(5))
	if err != nil {
		t.Fatal(err)
	}
	held, err := g.CaptureRead(testContext(t), earlier)
	if err != nil {
		t.Fatal(err)
	}
	testInstall(t, g, testMark(5), testMark(6))
	if err := testRebase(t, g, testMark(6), testMark(5), testMark(5).EnvelopeGeneration-1, ConfirmedInstalled); err != nil {
		t.Fatal(err)
	}
	if state := g.state.Load(); state.installed != testMark(5) || state.rebases != 1 || !g.MatchesInstalled(testMark(5)) {
		t.Fatal("fixture did not adopt the earlier owner again")
	}
	if err := g.WithWrite(stale, func(*WriteScope) error { t.Error("write attempt from before the split ran"); return nil }); !errors.Is(err, ErrMismatch) {
		t.Fatal("write attempt captured before a rebase was admitted for the same mark", err)
	}
	if err := g.WithRead(held, func(ReadScope) error { t.Error("registration from before the split read"); return nil }); !errors.Is(err, ErrRetired) {
		t.Fatal(err)
	}
	// Attempts captured after the rebase work as usual, and a rolled-back
	// rebase ends nothing.
	fresh := testBind(t, g, testFrame(testMark(5)))
	if fresh == earlier {
		t.Fatal("adopted owner reused its earlier registration")
	}
	survivor, err := g.CaptureWrite(testContext(t), testMark(5))
	if err != nil {
		t.Fatal(err)
	}
	if err := testRebase(t, g, testMark(5), testMark(3), 3, ConfirmedRollback); err != nil {
		t.Fatal(err)
	}
	if err := g.WithWrite(survivor, func(s *WriteScope) error { return s.Check() }); err != nil {
		t.Fatal("rolled-back rebase ended an unrelated attempt", err)
	}
	if err := readAt(testContext(t), t, g, fresh, func(s ReadScope) error { return s.Check() }); err != nil {
		t.Fatal(err)
	}
}
