package retirement

import (
	"errors"
	"math"
	"testing"
)

func TestMatchesInstalledRequiresActualManagedMarkWithoutRegistration(t *testing.T) {
	mark := testMark(1)
	var absent *Gate
	var zero Gate
	if absent.MatchesInstalled(mark) || zero.MatchesInstalled(mark) || testGate(t).MatchesInstalled(mark) {
		t.Fatal("missing installed state matched a claimed mark")
	}
	g := testGate(t)
	testInstall(t, g, Mark{}, mark)
	before := g.state.Load()
	if before.current != nil || !g.MatchesInstalled(mark) || g.state.Load() != before {
		t.Fatal("observer required/created a registration or mutated installed state")
	}
	err := writeAt(testContext(t), t, g, mark, func(scope *WriteScope) error {
		binding, err := scope.PrepareBinding(testFrame(mark))
		if err != nil {
			return err
		}
		if !g.MatchesInstalled(mark) || g.state.Load().current != nil {
			t.Fatal("first active publication could not observe source before Apply")
		}
		if err := binding.Apply(); err != nil {
			return err
		}
		if !g.MatchesInstalled(mark) {
			t.Fatal("applied unresolved publication hid installed source")
		}
		return binding.Resolve(ConfirmedPublished)
	})
	if err != nil || !g.MatchesInstalled(mark) {
		t.Fatal("confirmed publication source lost", err)
	}
}

func TestMatchesInstalledComparesEveryIdentityAndAcceptedBound(t *testing.T) {
	g := testGate(t)
	mark := testMark(1)
	testInstall(t, g, Mark{}, mark)
	mutations := []struct {
		name   string
		change func(*Mark)
	}{
		{"domain", func(m *Mark) { m.DomainID[1]++ }},
		{"envelope-digest", func(m *Mark) { m.EnvelopeDigest[1]++ }},
		{"pure-digest", func(m *Mark) { m.PureDigest[1]++ }},
		{"successor", func(m *Mark) { m.EnvelopeGeneration++; m.PureGeneration++ }},
		{"inconsistent-generation", func(m *Mark) { m.EnvelopeGeneration++ }},
		{"missing-domain", func(m *Mark) { m.DomainID = [32]byte{} }},
		{"missing-envelope-digest", func(m *Mark) { m.EnvelopeDigest = [32]byte{} }},
		{"missing-pure-digest", func(m *Mark) { m.PureDigest = [32]byte{} }},
	}
	before := g.state.Load()
	if g.MatchesInstalled(Mark{}) {
		t.Fatal("zero mark matched")
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			wrong := mark
			mutation.change(&wrong)
			if g.MatchesInstalled(wrong) || g.state.Load() != before {
				t.Fatal("observer matched/substituted claimed identity")
			}
		})
	}
	inconsistent := *before
	inconsistent.domain[0] ^= 1
	rawDomain := &Gate{}
	rawDomain.state.Store(&inconsistent)
	if rawDomain.MatchesInstalled(mark) {
		t.Fatal("installed mark bypassed separately retained domain")
	}
	for _, pure := range []uint64{0, 1, math.MaxUint64 - 2, math.MaxUint64 - 1, math.MaxUint64} {
		t.Run(testUintLabel(pure), func(t *testing.T) {
			candidate := testMark(pure)
			// A controlled raw mechanism state proves validation happens even
			// when all claimed fields equal the observed bytes.
			raw := &Gate{}
			raw.state.Store(&gateState{installed: candidate, domain: candidate.DomainID})
			expected := pure >= 1 && pure < math.MaxUint64-1
			if raw.MatchesInstalled(candidate) != expected {
				t.Fatal("accepted-compatible managed bound lost")
			}
		})
	}
}

func TestMatchesInstalledStagingAndRollbackDoNotInventSuccessor(t *testing.T) {
	g := testGate(t)
	first, next := testMark(1), testMark(2)
	testInstall(t, g, Mark{}, first)
	err := writeAt(testContext(t), t, g, first, func(scope *WriteScope) error {
		change, err := scope.BeginChange()
		if err != nil {
			return err
		}
		if err := change.Stage(next); err != nil {
			return err
		}
		if !g.MatchesInstalled(first) || g.MatchesInstalled(next) {
			t.Fatal("proposal changed observation before actual resolution")
		}
		return change.Resolve(ConfirmedRollback)
	})
	if err != nil || !g.MatchesInstalled(first) || g.MatchesInstalled(next) {
		t.Fatal("rollback changed installed observation", err)
	}
	testInstall(t, g, first, next)
	if g.MatchesInstalled(first) || !g.MatchesInstalled(next) {
		t.Fatal("confirmed successor did not retire original observation")
	}
}

func TestMatchesInstalledInclusiveDenialAndRestoredFloor(t *testing.T) {
	g := testGate(t)
	first, next := testMark(1), testMark(2)
	testInstall(t, g, Mark{}, first)
	err := writeAt(testContext(t), t, g, first, func(scope *WriteScope) error {
		denial, err := scope.PrepareWithdrawal()
		if err != nil {
			return err
		}
		if err := denial.Resolve(ConfirmedDurableDeny); err != nil {
			return err
		}
		if g.MatchesInstalled(first) {
			t.Fatal("inclusive denial still matched installed owner")
		}
		return nil
	})
	if err != nil || g.MatchesInstalled(first) {
		t.Fatal("resolved denial lost", err)
	}
	testInstall(t, g, first, next)
	if !g.MatchesInstalled(next) {
		t.Fatal("successor above retained floor refused")
	}
	for _, floor := range []uint64{first.EnvelopeGeneration - 1, first.EnvelopeGeneration, math.MaxUint64} {
		t.Run(testUintLabel(floor), func(t *testing.T) {
			restored, err := New(testHistory(), first.DomainID, floor)
			if err != nil {
				t.Fatal(err)
			}
			testInstall(t, restored, Mark{}, first)
			if restored.MatchesInstalled(first) != (floor < first.EnvelopeGeneration) {
				t.Fatal("restored inclusive/max floor ignored")
			}
		})
	}
}

func TestMatchesInstalledGateClosureAndUnresolvedPlanRefuse(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "unresolved"}[unresolved], func(t *testing.T) {
			g := testGate(t)
			mark := testMark(1)
			testInstall(t, g, Mark{}, mark)
			if unresolved {
				err := writeAt(testContext(t), t, g, mark, func(scope *WriteScope) error {
					_, err := scope.BeginChange()
					return err // deliberately unresolved private mechanism plan
				})
				if !errors.Is(err, ErrClosed) {
					t.Fatal("unresolved fixture did not close", err)
				}
			} else {
				g.Close()
			}
			if g.MatchesInstalled(mark) {
				t.Fatal("closed mechanism still matched")
			}
		})
	}
}

func TestMatchesInstalledObserverDoesNotAllocateOrTakeQueueMutex(t *testing.T) {
	g := testGate(t)
	mark := testMark(1)
	testInstall(t, g, Mark{}, mark)
	before := g.state.Load()
	allocations := testing.AllocsPerRun(100, func() {
		if !g.MatchesInstalled(mark) {
			panic("fixture: current installed source refused")
		}
	})
	if allocations != 0 || g.state.Load() != before {
		t.Fatal("observer allocated or mutated state", allocations)
	}
	ctx := testContext(t)
	observed := make(chan bool, 1)
	g.mu.Lock()
	go func() { observed <- g.MatchesInstalled(mark) }()
	var result bool
	timedOut := false
	select {
	case result = <-observed:
	case <-ctx.Done():
		timedOut = true
	}
	g.mu.Unlock()
	if timedOut {
		// The queue lock has been released; the buffered observation can finish
		// without a blocked send even on the negative implementation path.
		t.Fatal("observer took queue mutex")
	}
	if !result {
		t.Fatal("queue mutex changed installed observation")
	}
}
