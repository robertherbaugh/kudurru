package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// These tests compose the parent's actual schema8 fresh/Dormant/local helpers.
// No copied migration, backward marker, missing-floor repair or received-image
// conversion belongs in this file. Detached schema7 is not qualification.
func anchorPublicationTestCapture(ctx context.Context, t *testing.T, s *Store) (GenerationToken, *AnchorPublication) {
	t.Helper()
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := s.CaptureAnchorPublication(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	return token, publication
}

func anchorPublicationTestPublish(ctx context.Context, t *testing.T, s *Store) *AnchorResolverInput {
	t.Helper()
	token, p := anchorPublicationTestCapture(ctx, t, s)
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, p.Apply); err != nil {
		t.Fatal(err)
	}
	return p.Input()
}

func anchorPublicationTestRead(ctx context.Context, t *testing.T, input *AnchorResolverInput) {
	t.Helper()
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = input.WithRead(read, func(scope AnchorReadScope) error {
		called = true
		return scope.Check()
	})
	if err != nil || !called {
		t.Fatal("original read refused", err)
	}
}

func anchorPublicationTestMutation(ctx context.Context, t *testing.T, s *Store) {
	t.Helper()
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.PutSection(ctx, "runtime", replication.Entry{Key: "publication-fixture/cut", Data: []byte(`{"changed":true}`)})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAnchorPublicationCapturesActualCommittedInputWithoutWrites(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	before := imageOf(t, s).Generation()
	local := anchorLocalTestRead(t, s)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	c := p.cell
	if c.source != source || c.cursor != source.cursor.Load() || c.original != ctx || !c.token.generation.Same(before) || !c.token.Matches(s.Authority()) {
		t.Fatal("publication did not retain actual source/owner/original token")
	}
	if p.Projection().Disposition() != resolverstate.Usable || p.Projection().Identity().Owner != proof.next.Version() || p.Projection().OwnerID() == ([32]byte{}) {
		t.Fatal("committed projection identity missing")
	}
	ds := p.Projection().DS()
	ds[0] = "detached"
	if reflect.DeepEqual(ds, p.Projection().DS()) {
		t.Fatal("projection exposed mutable keys")
	}
	if _, err := p.Input().CaptureRead(ctx); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("captured bytes registered a read", err)
	}
	if err := p.Apply(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("Apply escaped callback", err)
	}
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, p.Apply); err != nil {
		t.Fatal(err)
	}
	anchorPublicationTestRead(ctx, t, p.Input())
	if !before.Same(imageOf(t, s).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) || source.cursor.Load() != c.cursor {
		t.Fatal("publication altered canonical owner/local/authority")
	}
}

func TestAnchorPublicationRegistrationSurvivesOriginalWorkCancellation(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx, cancel := context.WithCancel(anchorLocalTestFinite(t.Context(), t))
	defer cancel()
	token, p := anchorPublicationTestCapture(ctx, t, s)
	swapped := false
	err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
		if err := p.Apply(); err != nil {
			return err
		}
		swapped = true // stand-in for the future infallible parent bundle swap
		cancel()       // post-effect cancellation cannot become rollback
		return nil
	})
	if err != nil || !swapped || p.Input().cell.snapshot.Load() == nil {
		t.Fatal("confirmed publication became rollback", err)
	}
	anchorPublicationTestRead(anchorLocalTestFinite(t.Context(), t), t, p.Input())
	if _, err := p.Input().CaptureRead(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("query substituted publication lifetime", err)
	}
	if err := p.Apply(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("escaped Apply mutated after release", err)
	}
	if err := s.PublishIfCurrent(anchorLocalTestFinite(t.Context(), t), token, PublicationOptions{Anchor: p}, p.Apply); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("published operation replayed", err)
	}
}

func TestAnchorPublicationBeforeApplyRefusalPreservesExistingRegistration(t *testing.T) {
	for _, noApply := range []bool{false, true} {
		t.Run(map[bool]string{false: "callback-error", true: "nil-without-apply"}[noApply], func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			ctx := anchorLocalTestFinite(t.Context(), t)
			old := anchorPublicationTestPublish(ctx, t, s)
			token, p := anchorPublicationTestCapture(ctx, t, s)
			refusal := errors.New("fixture: dependency interposed before swap")
			err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
				if noApply {
					return nil
				}
				return refusal
			})
			if noApply && !errors.Is(err, retirement.ErrScope) || !noApply && !errors.Is(err, refusal) {
				t.Fatal("pre-Apply refusal lost", err)
			}
			if source.closed.Load() || p.Input().cell.snapshot.Load() != nil {
				t.Fatal("no-effect callback granted input or closed prior input")
			}
			anchorPublicationTestRead(ctx, t, old)
			if err := p.Apply(); !errors.Is(err, retirement.ErrScope) {
				t.Fatal("refused callback remained active", err)
			}
		})
	}
}

func TestAnchorPublicationAppliedCallbackErrorClosesWithoutRollbackCredit(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	old := anchorPublicationTestPublish(ctx, t, s)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	refusal := errors.New("fixture: error after actual swap")
	swapped := false
	err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
		if err := p.Apply(); err != nil {
			return err
		}
		swapped = true
		return refusal
	})
	var effect *retirement.EffectError
	if !swapped || !errors.Is(err, refusal) || !errors.As(err, &effect) || effect.Effect() != retirement.PublicationAppliedEffect {
		t.Fatal("applied effect mislabeled clean refusal", err)
	}
	if !source.closed.Load() || !s.anchorBlocked.Load() || p.Input().cell.snapshot.Load() != nil {
		t.Fatal("uncertain publication stayed open")
	}
	if _, err := old.CaptureRead(ctx); err == nil {
		t.Fatal("prior registration survived ambiguous swap")
	}
}

func TestAnchorPublicationPanicAfterApplyClosesBeforeRelease(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	marker := errors.New("fixture: panic after swap")
	recovered := func() (value any) {
		defer func() { value = recover() }()
		_ = s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
			if err := p.Apply(); err != nil {
				return err
			}
			panic(marker)
		})
		return nil
	}()
	if recovered != marker || !source.closed.Load() || p.Input().cell.snapshot.Load() != nil {
		t.Fatal("panic lost or admission left open")
	}
	if !s.wmu.TryLock() {
		t.Fatal("panic retained wmu")
	}
	s.wmu.Unlock()
	if err := p.Apply(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("panic left Apply live", err)
	}
}

func TestAnchorPublicationCopiesShareOneExecutionAndNoEscapedSuccessorMutation(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	copied := *p
	var calls atomic.Int32
	results := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for _, op := range []*AnchorPublication{p, &copied} {
		go func() {
			start.Wait()
			results <- s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: op}, func() error {
				calls.Add(1)
				return op.Apply()
			})
		}()
	}
	start.Done()
	success, reused := 0, 0
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				success++
			case errors.Is(err, retirement.ErrUsed):
				reused++
			default:
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("original publication wait expired", ctx.Err())
		}
	}
	if success != 1 || reused != 1 || calls.Load() != 1 {
		t.Fatal("copies replayed original callback")
	}
	newer := anchorPublicationTestPublish(ctx, t, s)
	if err := copied.Apply(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("escaped Apply touched successor", err)
	}
	if source.closed.Load() {
		t.Fatal("escaped Apply poisoned successor")
	}
	anchorPublicationTestRead(ctx, t, newer)
}

func TestAnchorPublicationDuplicateApplyAndInFlightReturnAreUnknown(t *testing.T) {
	for _, kind := range []string{"duplicate", "in-flight-at-return"} {
		t.Run(kind, func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			ctx := anchorLocalTestFinite(t.Context(), t)
			token, p := anchorPublicationTestCapture(ctx, t, s)
			err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
				if kind == "duplicate" {
					if err := p.Apply(); err != nil {
						return err
					}
					if err := p.Apply(); !errors.Is(err, retirement.ErrUsed) {
						return errors.New("fixture: duplicate Apply not refused")
					}
				} else {
					// Deterministic private CAS boundary: a winning Apply has
					// claimed the callback cell but has not reported completion.
					old := p.cell.call.Load()
					if old == nil || !p.cell.call.CompareAndSwap(old, &old.nodes.applying) {
						return errors.New("fixture: missing active callback state")
					}
				}
				return nil
			})
			if err == nil || !source.closed.Load() || p.Input().cell.snapshot.Load() != nil {
				t.Fatal("ambiguous Apply got rollback/publication credit", err)
			}
			if err := p.Apply(); !errors.Is(err, retirement.ErrScope) {
				t.Fatal("closed callback remained mutable", err)
			}
		})
	}
}

func TestAnchorPublicationSameFrameRepublishSharesOnlyMechanicalSnapshot(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	first := anchorPublicationTestPublish(ctx, t, s)
	second := anchorPublicationTestPublish(ctx, t, s)
	if first == second || first.cell == second.cell || first.cell.snapshot.Load() != second.cell.snapshot.Load() {
		t.Fatal("same-frame publication mixed complete input identity with mechanical registration")
	}
	anchorPublicationTestRead(ctx, t, first)
	anchorPublicationTestRead(ctx, t, second)
}

func TestAnchorPublicationOriginalFiniteLifetimeCannotBeReplaced(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	good := anchorLocalTestFinite(t.Context(), t)
	_, token, err := s.ReadServingCandidate(good)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(good)
	cancel()
	expired, stopExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stopExpired()
	long, stopLong := context.WithTimeout(t.Context(), time.Hour)
	defer stopLong()
	cases := []struct {
		name string
		ctx  context.Context
		want error
	}{
		{"nil", nil, replication.ErrInvalid},
		{"no-deadline", context.Background(), retirement.ErrInvalid},
		{"long-finite", long, retirement.ErrInvalid},
		{"canceled", canceled, context.Canceled},
		{"expired", expired, context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.CaptureAnchorPublication(tc.ctx, token); !errors.Is(err, tc.want) {
				t.Fatal("original lifetime category lost", err)
			}
		})
	}
	original, cancelOriginal := context.WithCancel(good)
	p, err := s.CaptureAnchorPublication(original, token)
	if err != nil {
		t.Fatal(err)
	}
	cancelOriginal()
	called := false
	err = s.PublishIfCurrent(context.WithoutCancel(original), token, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
	if !errors.Is(err, context.Canceled) || called {
		t.Fatal("detached work context rescued original", err)
	}
}

func TestAnchorPublicationForeignZeroAndSubstitutedTokenRefuse(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	other, _, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	if _, err := other.CaptureAnchorPublication(ctx, token); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign token captured", err)
	}
	if _, err := s.CaptureAnchorPublication(ctx, GenerationToken{}); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("zero token captured", err)
	}
	if err := other.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, p.Apply); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign Store accepted operation", err)
	}
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: &AnchorPublication{}}, p.Apply); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("zero operation accepted", err)
	}
	substitute := token
	substitute.generation.Digest = "substituted"
	called := false
	err := s.PublishIfCurrent(ctx, substitute, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
	if !errors.Is(err, replication.ErrCAS) || called {
		t.Fatal("replacement token rescued original", err)
	}
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, p.Apply); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("refused operation replayed", err)
	}
	if (&AnchorPublication{}).Projection().Disposition() != resolverstate.Invalid || (&AnchorPublication{}).Input() != nil {
		t.Fatal("zero publication looked usable")
	}
}

func TestAnchorPublicationCaptureAndEntryRejectInterposedActualCuts(t *testing.T) {
	for _, kind := range []string{"before-capture", "installed", "writer", "local-denial", "owner", "local-sql", "unknown-owner-row"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			ctx := anchorTestContext(t.Context(), t, s)
			token, p := anchorPublicationTestCapture(ctx, t, s)
			before := source.cursor.Load()
			switch kind {
			case "before-capture", "installed":
				anchorPublicationTestMutation(ctx, t, s)
			case "writer":
				if _, err := s.EnablePrimary(ctx, token.Generation()); err != nil {
					t.Fatal(err)
				}
			case "local-denial":
				_, _, verified := anchorLocalTestRevoke(t, s, proof)
				if result, _, err := s.CommitAnchorDenial(verified); err != nil || result.Outcome() != AnchorCommitted {
					t.Fatal("actual deny fixture", err)
				}
			case "owner":
				update, err := s.CaptureAnchorUpdate(ctx, proof.next.Version())
				if err != nil {
					t.Fatal(err)
				}
				observation, err := trustanchors.Verify(proof.state, anchorTestSigned(t, proof.key, proof.signer, false), proof.clock.Sample())
				if err != nil {
					t.Fatal(err)
				}
				result, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
					return anchorLocalTestAdvance(ctx, tx, observation, proof)
				})
				if err != nil || result.Outcome() != AnchorCommitted {
					t.Fatal("actual owner fixture", err)
				}
			case "local-sql":
				// Controlled physical fault: actual local bytes change without
				// the memory cursor. Final admission must reread the carrier.
				record := before.local.record
				record.Revision++
				local, err := encodeAnchorLocal(record)
				if err != nil {
					t.Fatal(err)
				}
				result, err := s.w.ExecContext(ctx, `UPDATE meta SET value=? WHERE key=? AND value=?`, local.raw, anchorLocalKey, before.local.raw)
				if err != nil {
					t.Fatal(err)
				}
				changed, err := result.RowsAffected()
				if err != nil || changed != 1 {
					t.Fatal("local physical fault was not reached", err)
				}
			case "unknown-owner-row":
				// An actual physical unknown-version row must not be hidden by
				// a structurally valid in-memory projection or absent validator.
				result, err := s.w.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, accepted.Prefix+"v999/unknown", `{}`)
				if err != nil {
					t.Fatal(err)
				}
				changed, err := result.RowsAffected()
				if err != nil || changed != 1 {
					t.Fatal("unknown-owner physical fault was not reached", err)
				}
			}
			if kind == "before-capture" {
				if _, err := s.CaptureAnchorPublication(ctx, token); !errors.Is(err, replication.ErrCAS) {
					t.Fatal("old token captured current owner", err)
				}
				return
			}
			if kind == "installed" && source.cursor.Load() != before {
				t.Fatal("installed fixture changed anchor cursor too")
			}
			called := false
			err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
			if err == nil || called || p.Input().cell.snapshot.Load() != nil {
				t.Fatal("interposed actual cut was adopted", err)
			}
		})
	}
}

func TestAnchorPublicationReadAttemptsAreOpaqueBoundAndOneShot(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	first := anchorPublicationTestPublish(ctx, t, s)
	second := anchorPublicationTestPublish(ctx, t, s)
	read, err := first.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copied := *read
	called := false
	if err := second.WithRead(read, func(AnchorReadScope) error { called = true; return nil }); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("foreign input adopted read", err)
	}
	var escaped AnchorReadScope
	err = first.WithRead(read, func(scope AnchorReadScope) error {
		called = true
		escaped = scope
		if !s.wmu.TryLock() {
			return errors.New("fixture: query unexpectedly holds wmu")
		}
		s.wmu.Unlock()
		return scope.Check()
	})
	if err != nil || !called {
		t.Fatal("original mechanical read refused", err)
	}
	if err := escaped.Check(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("borrowed predicate escaped exclusion", err)
	}
	if err := first.WithRead(&copied, func(AnchorReadScope) error { return nil }); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("copied read replayed", err)
	}
	if _, err := (&AnchorResolverInput{}).CaptureRead(ctx); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("zero input captured", err)
	}
	if err := (AnchorReadScope{}).Check(); !errors.Is(err, retirement.ErrScope) {
		t.Fatal("zero scope checked", err)
	}
}

func TestAnchorPublicationOriginalQueryCancellationAndFiniteBounds(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	publication := anchorLocalTestFinite(t.Context(), t)
	input := anchorPublicationTestPublish(publication, t, s)
	if _, err := input.CaptureRead(context.Background()); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("unbounded query admitted", err)
	}
	long, stopLong := context.WithTimeout(t.Context(), time.Hour)
	defer stopLong()
	if _, err := input.CaptureRead(long); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long query admitted", err)
	}
	original, cancel := context.WithCancel(anchorLocalTestFinite(t.Context(), t))
	read, err := input.CaptureRead(original)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	called := false
	if err := input.WithRead(read, func(AnchorReadScope) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatal("read lost original cancellation", err)
	}
	deadline, stopDeadline := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer stopDeadline()
	read, err = input.CaptureRead(deadline)
	if err != nil {
		t.Fatal(err)
	}
	<-deadline.Done()
	if err := input.WithRead(read, func(AnchorReadScope) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("read lost original deadline", err)
	}
}

func TestAnchorPublicationReaderHoldsRetirementBeforeActualCommand(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorTestContext(t.Context(), t, s)
	input := anchorPublicationTestPublish(ctx, t, s)
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	update, err := s.CaptureAnchorUpdate(ctx, proof.next.Version())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(proof.state, anchorTestSigned(t, proof.key, proof.signer, false), proof.clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	attempted := make(chan struct{})
	done := make(chan error, 1)
	err = input.WithRead(read, func(scope AnchorReadScope) error {
		go func() {
			close(attempted)
			_, commandErr := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
				return anchorLocalTestAdvance(ctx, tx, observation, proof)
			})
			done <- commandErr
		}()
		select {
		case <-attempted:
		case <-ctx.Done():
			return ctx.Err()
		}
		timer := time.NewTimer(20 * time.Millisecond)
		defer timer.Stop()
		select {
		case commandErr := <-done:
			return errors.Join(errors.New("fixture: writer crossed read hold"), commandErr)
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !s.wmu.TryLock() {
			return errors.New("fixture: writer acquired wmu before R")
		}
		s.wmu.Unlock()
		return scope.Check()
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("writer did not finish after read release", ctx.Err())
	}
	if source.cursor.Load().owner.Version() == proof.next.Version() {
		t.Fatal("actual command did not advance owner")
	}
	if _, err := input.CaptureRead(ctx); err == nil {
		t.Fatal("old registration survived committed successor")
	}
}

func TestAnchorPublicationFollowerReadRequiresNoWriterAndLocalDenyRetires(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	ctx := anchorLocalTestFinite(t.Context(), t)
	if err := anchorLocalTestInstall(ctx, t, follower, primary); err != nil {
		t.Fatal(err)
	}
	input := anchorPublicationTestPublish(ctx, t, follower)
	read, err := input.CaptureRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := WriterTokenFromContext(ctx); ok {
		t.Fatal("follower publication unexpectedly carried writer")
	}
	before := imageOf(t, follower).Generation()
	denial, err := follower.CaptureAnchorDenial(ctx, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := follower.CommitAnchorDenial(verified)
	if err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal(err)
	}
	called := false
	if err := input.WithRead(read, func(AnchorReadScope) error { called = true; return nil }); err == nil || called {
		t.Fatal("captured old read crossed durable denial", err)
	}
	if !before.Same(imageOf(t, follower).Generation()) || source.cursor.Load().owner.Version() != proof.next.Version() {
		t.Fatal("follower publication/denial invented canonical progress")
	}
	_, token, err := follower.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("local deny floor reset by publication", err)
	}
}

func TestAnchorPublicationRejectsAwaitingDormantAndQuarantinedOwners(t *testing.T) {
	s := openTemp(t)              // raw registry permits reading the genuine physical sentinel
	anchorLocalTestAwaiting(t, s) // parent schema8 assertion; never repair
	anchorTestBind(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	_, awaiting, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureAnchorPublication(ctx, awaiting); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("sentinel registered input", err)
	}
	_, dormant := anchorLocalTestDormant(t, s)
	_, token, err := s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Dormant fell back to root defaults", err)
	}
	proof := anchorTestManaged(t, s, dormant)
	_, update, verified := anchorLocalTestRevoke(t, s, proof)
	_, own, err := s.CommitAnchorDenial(verified)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.AnchorCommandAfterDenial(update, own, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	})
	if err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal(err)
	}
	_, token, err = s.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("quarantine fell back to static roots", err)
	}
}

func TestAnchorPublicationRestartRequiresFreshCommittedRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	ctx := anchorTestContext(t.Context(), t, s)
	old := anchorPublicationTestPublish(ctx, t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions)
	original := anchorTestContext(t.Context(), t, reopened)
	source, err := reopened.BindAnchorLifecycle(original)
	if err != nil {
		t.Fatal(err)
	}
	if source.cursor.Load().owner.Version() != proof.next.Version() {
		t.Fatal("restart changed accepted owner")
	}
	if _, err := old.CaptureRead(original); err == nil {
		t.Fatal("old Store input survived restart")
	}
	token, p := anchorPublicationTestCapture(original, t, reopened)
	if _, err := p.Input().CaptureRead(original); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("restored gate assumed published input", err)
	}
	if err := reopened.PublishIfCurrent(original, token, PublicationOptions{Anchor: p}, p.Apply); err != nil {
		t.Fatal(err)
	}
	anchorPublicationTestRead(original, t, p.Input())
}

func TestAnchorPublicationRetainedLocalFloorSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	_, _, verified := anchorLocalTestRevoke(t, s, proof)
	if result, _, err := s.CommitAnchorDenial(verified); err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal(err)
	}
	denied := anchorLocalTestRead(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions)
	ctx := anchorTestContext(t.Context(), t, reopened)
	if _, err := reopened.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	_, token, err := reopened.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.CaptureAnchorPublication(ctx, token); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("reopen inferred zero local floor", err)
	}
	if !sameAnchorLocal(denied, anchorLocalTestRead(t, reopened)) {
		t.Fatal("publication repaired retained denial")
	}
}

func TestAnchorPublicationOrdinaryInertPolicyCheckDoesNotRegisterInput(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if p.cell.used.Load() || p.Input().cell.snapshot.Load() != nil {
		t.Fatal("inert policy check consumed or activated pending input")
	}
	if err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, p.Apply); err != nil {
		t.Fatal(err)
	}
	anchorPublicationTestRead(ctx, t, p.Input())
	if p.Projection().Identity().Owner == (accepted.Version{}) {
		t.Fatal("actual owner identity missing")
	}
}

func TestAnchorPublicationHeldAndFinalOriginalExpiryRefuseWithoutRenewal(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "held-before-entry", true: "inside-final-callback"}[final], func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			preparation := anchorLocalTestFinite(t.Context(), t)
			// The original child precedes candidate acquisition as well as
			// capture; the outer context cannot rescue its later expiration.
			original, stop := context.WithTimeout(preparation, 250*time.Millisecond)
			defer stop()
			_, token, err := s.ReadServingCandidate(original)
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.CaptureAnchorPublication(original, token)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			if !final {
				<-original.Done()
			}
			err = s.PublishIfCurrent(preparation, token, PublicationOptions{Anchor: p}, func() error {
				called = true
				<-original.Done() // controlled pre-Apply hold, no source restamp
				return p.Apply()
			})
			if !errors.Is(err, context.DeadlineExceeded) || called != final || source.closed.Load() || p.Input().cell.snapshot.Load() != nil {
				t.Fatal("original expired publication admitted/renewed or lost category", err)
			}
		})
	}
}

type anchorPublicationTestContext struct {
	context.Context
	final atomic.Bool
	calls atomic.Int32
}

func (c *anchorPublicationTestContext) Deadline() (time.Time, bool) {
	if c.final.Load() {
		c.calls.Add(1)
	}
	return c.Context.Deadline()
}

func (c *anchorPublicationTestContext) Done() <-chan struct{} {
	if c.final.Load() {
		c.calls.Add(1)
	}
	return c.Context.Done()
}

func (c *anchorPublicationTestContext) Err() error {
	if c.final.Load() {
		c.calls.Add(1)
	}
	return c.Context.Err()
}

func TestAnchorPublicationApplyUsesNoContextInterfaceInFinalCallback(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx := &anchorPublicationTestContext{Context: anchorLocalTestFinite(t.Context(), t)}
	token, p := anchorPublicationTestCapture(ctx, t, s)
	err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
		ctx.final.Store(true)
		result := p.Apply()
		ctx.final.Store(false)
		return result
	})
	if err != nil || ctx.calls.Load() != 0 {
		t.Fatal("Apply invoked arbitrary context methods", err)
	}
	anchorPublicationTestRead(anchorLocalTestFinite(t.Context(), t), t, p.Input())
}

func TestAnchorPublicationConcurrentApplyCannotResolveAsSafeRollback(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	token, p := anchorPublicationTestCapture(ctx, t, s)
	var successes atomic.Int32
	err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
		start := make(chan struct{})
		results := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				results <- p.Apply()
			}()
		}
		close(start)
		for range 2 {
			select {
			case err := <-results:
				if err == nil {
					successes.Add(1)
				} else if !errors.Is(err, retirement.ErrUsed) && !errors.Is(err, retirement.ErrScope) {
					return err
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
	if err == nil || successes.Load() > 1 || !source.closed.Load() || p.Input().cell.snapshot.Load() != nil {
		t.Fatal("concurrent Apply produced authority or clean rollback", err)
	}
}
