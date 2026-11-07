package sqlite

import (
	"context"
	"crypto/sha256"
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

func TestAnchorPublicationCurrentBeforeApplyAndAfterWorkCancellation(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx, cancel := context.WithCancel(anchorLocalTestFinite(t.Context(), t))
	defer cancel()
	token, p := anchorPublicationTestCapture(ctx, t, s)
	input := p.Input()
	domain := input.CacheDomain()
	if !input.Current() || input.cell.snapshot.Load() != nil || domain == ([32]byte{}) {
		t.Fatal("source Current required an applied publication")
	}
	if _, err := input.CaptureRead(ctx); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Current manufactured a registration", err)
	}
	err := s.PublishIfCurrent(ctx, token, PublicationOptions{Anchor: p}, func() error {
		if !input.Current() {
			return errors.New("fixture: first publication source predicate refused")
		}
		if err := p.Apply(); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if err != nil || !input.Current() || input.CacheDomain() != domain {
		t.Fatal("publication work cancellation expired/restamped current source", err)
	}
	anchorPublicationTestRead(anchorLocalTestFinite(t.Context(), t), t, input)
}

func TestAnchorPublicationCurrentIsSourceOnlyAfterUnpublishedOriginalExpires(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	original, cancel := context.WithCancel(anchorLocalTestFinite(t.Context(), t))
	token, p := anchorPublicationTestCapture(original, t, s)
	input := p.Input()
	cancel()
	if !input.Current() || input.cell.snapshot.Load() != nil {
		t.Fatal("source currency confused with work lifetime or publication")
	}
	called := false
	err := s.PublishIfCurrent(context.WithoutCancel(original), token, PublicationOptions{Anchor: p}, func() error { called = true; return p.Apply() })
	if !errors.Is(err, context.Canceled) || called || !input.Current() {
		t.Fatal("source predicate renewed expired original or lost source", err)
	}
	if _, err := input.CaptureRead(anchorLocalTestFinite(t.Context(), t)); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("unpublished Current became a read grant", err)
	}
}

func TestAnchorPublicationCurrentIncludesGateOnlyClosure(t *testing.T) {
	for _, unresolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-gate-close", true: "unresolved-plan"}[unresolved], func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			ctx := anchorLocalTestFinite(t.Context(), t)
			_, p := anchorPublicationTestCapture(ctx, t, s)
			input := p.Input()
			domain := input.CacheDomain()
			before := source.cursor.Load()
			if unresolved {
				attempt, err := before.gate.CaptureWrite(ctx, before.mark)
				if err != nil {
					t.Fatal(err)
				}
				err = before.gate.WithWrite(attempt, func(scope *retirement.WriteScope) error {
					_, err := scope.BeginChange()
					return err // actual unresolved scope-close, no driver flags
				})
				if !errors.Is(err, retirement.ErrClosed) {
					t.Fatal("unresolved plan did not reach close", err)
				}
			} else {
				before.gate.Close()
			}
			if source.closed.Load() || s.anchorBlocked.Load() || source.cursor.Load() != before {
				t.Fatal("gate-only fixture also changed driver flags/cursor")
			}
			if input.Current() || input.CacheDomain() != domain {
				t.Fatal("Current missed gate closure or identity restamped")
			}
		})
	}
}

func TestAnchorPublicationCurrentRefusesSourceFlagsAndForeignCursor(t *testing.T) {
	for _, kind := range []string{"write-blocked", "anchor-blocked", "source-closed", "foreign-source", "foreign-cursor"} {
		t.Run(kind, func(t *testing.T) {
			s, source, _ := anchorLocalTestManaged(t)
			_, p := anchorPublicationTestCapture(anchorLocalTestFinite(t.Context(), t), t, s)
			input := p.Input()
			domain := input.CacheDomain()
			switch kind {
			case "write-blocked":
				s.writeBlocked.Store(true)
			case "anchor-blocked":
				s.anchorBlocked.Store(true)
			case "source-closed":
				source.closed.Store(true)
			case "foreign-source":
				other, _, _ := anchorLocalTestManaged(t)
				// Controlled private input substitution, never a public constructor.
				input = &AnchorResolverInput{cell: &anchorResolverInput{source: other.anchors.Load(), cursor: source.cursor.Load(), cacheDomain: domain}}
			case "foreign-cursor":
				other, _, _ := anchorLocalTestManaged(t)
				input = &AnchorResolverInput{cell: &anchorResolverInput{source: source, cursor: other.anchors.Load().cursor.Load(), cacheDomain: domain}}
			}
			if input.Current() || input.CacheDomain() != domain {
				t.Fatal("invalid source became current or identity followed successor")
			}
		})
	}
	var absent *AnchorResolverInput
	if absent.Current() || (&AnchorResolverInput{}).Current() || absent.CacheDomain() != ([32]byte{}) || (&AnchorResolverInput{}).CacheDomain() != ([32]byte{}) {
		t.Fatal("zero input looked current/nonzero")
	}
}

func TestAnchorPublicationCurrentAndDomainDoNoContextCallsOrAllocations(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	original := &anchorPublicationTestContext{Context: anchorLocalTestFinite(t.Context(), t)}
	_, p := anchorPublicationTestCapture(original, t, s)
	input := p.Input()
	domain := input.CacheDomain()
	cursor := input.cell.cursor
	local := anchorLocalTestRead(t, s)
	original.final.Store(true)
	allocations := testing.AllocsPerRun(100, func() {
		if !input.Current() || input.CacheDomain() != domain {
			panic("fixture: source observer changed")
		}
	})
	original.final.Store(false)
	if allocations != 0 || original.calls.Load() != 0 || input.cell.cursor != cursor || input.cell.snapshot.Load() != nil || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
		t.Fatal("source observers allocated/called context/mutated committed input", allocations)
	}
}

func TestAnchorPublicationCacheDomainBindsFullCapturedOwnerLocalAndNonce(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	_, p := anchorPublicationTestCapture(anchorLocalTestFinite(t.Context(), t), t, s)
	input := p.Input()
	projection := p.Projection()
	domainID, ownerID := projection.DomainID(), projection.OwnerID()
	local := anchorLocalTestRead(t, s)
	// Independent specification encoding checks the actual capture wiring,
	// including full accepted/local identities rather than only effective DS.
	raw := []byte("kudurru/managed-root-cache/v1\x00")
	raw = append(raw, domainID[:]...)
	raw = append(raw, ownerID[:]...)
	raw = append(raw, local.digest[:]...)
	raw = append(raw, input.cell.cacheNonce[:]...)
	if input.CacheDomain() != sha256.Sum256(raw) {
		t.Fatal("cache domain omitted/substituted captured identity")
	}
	fields := [4][32]byte{domainID, ownerID, local.digest, input.cell.cacheNonce}
	names := []string{"domain", "full-owner", "local-carrier", "input-nonce"}
	for index, name := range names {
		t.Run(name, func(t *testing.T) {
			changed := fields
			changed[index][0] ^= 1
			digest := anchorCacheDomain(changed[0], changed[1], changed[2], changed[3])
			if digest == input.CacheDomain() {
				t.Fatal("complete field omitted from cache identity")
			}
		})
	}
	changedRecord := local.record
	changedRecord.Revision++
	changedLocal, err := encodeAnchorLocal(changedRecord)
	if err != nil {
		t.Fatal(err)
	}
	if changedLocal.digest == local.digest || anchorCacheDomain(domainID, ownerID, changedLocal.digest, input.cell.cacheNonce) == input.CacheDomain() {
		t.Fatal("same-owner local revision was reduced to accepted/DS identity")
	}
}

func TestAnchorPublicationEqualOwnerInputsReceiveDistinctFixedCacheDomains(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	firstToken, first := anchorPublicationTestCapture(ctx, t, s)
	secondToken, second := anchorPublicationTestCapture(ctx, t, s)
	a, b := first.Input(), second.Input()
	if a.cell.cursor != b.cell.cursor || first.Projection().OwnerID() != second.Projection().OwnerID() || !sameAnchorLocal(a.cell.cursor.local, b.cell.cursor.local) {
		t.Fatal("equal-owner fixture changed source")
	}
	if a.cell.cacheNonce == b.cell.cacheNonce || a.CacheDomain() == b.CacheDomain() || a.CacheDomain() == ([32]byte{}) || b.CacheDomain() == ([32]byte{}) {
		t.Fatal("equal-source captures reused cache incarnation")
	}
	domainA, domainB := a.CacheDomain(), b.CacheDomain()
	if err := s.PublishIfCurrent(ctx, firstToken, PublicationOptions{Anchor: first}, first.Apply); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishIfCurrent(ctx, secondToken, PublicationOptions{Anchor: second}, second.Apply); err != nil {
		t.Fatal(err)
	}
	if a.cell.snapshot.Load() != b.cell.snapshot.Load() || a.CacheDomain() != domainA || b.CacheDomain() != domainB || !a.Current() || !b.Current() {
		t.Fatal("mechanical same-frame reuse conflated cache domains or changed source")
	}
}

func TestAnchorPublicationCurrentAndCacheDomainRetainOriginalOnOwnerAdvance(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorTestContext(t.Context(), t, s)
	_, old := anchorPublicationTestCapture(ctx, t, s)
	before := old.Input().CacheDomain()
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
		t.Fatal(err)
	}
	_, current := anchorPublicationTestCapture(ctx, t, s)
	if !reflect.DeepEqual(old.Projection().DS(), current.Projection().DS()) || old.Projection().OwnerID() == current.Projection().OwnerID() || source.cursor.Load().owner.Version() == proof.next.Version() {
		t.Fatal("checkpoint-only fixture did not preserve keys/change full owner")
	}
	if old.Input().Current() || old.Input().CacheDomain() != before || !current.Input().Current() || current.Input().CacheDomain() == before {
		t.Fatal("old input followed checkpoint successor")
	}
	fixedNonce := old.Input().cell.cacheNonce
	oldSameNonce := anchorCacheDomain(old.Projection().DomainID(), old.Projection().OwnerID(), old.Input().cell.cursor.local.digest, fixedNonce)
	newSameNonce := anchorCacheDomain(current.Projection().DomainID(), current.Projection().OwnerID(), current.Input().cell.cursor.local.digest, fixedNonce)
	if oldSameNonce == newSameNonce {
		t.Fatal("identity change depended only on random incarnation")
	}
}

func TestAnchorPublicationLocalDenyMakesCurrentFalseWithoutCanonicalProgress(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	ctx := anchorLocalTestFinite(t.Context(), t)
	_, p := anchorPublicationTestCapture(ctx, t, s)
	input := p.Input()
	oldDomain := input.CacheDomain()
	before := imageOf(t, s).Generation()
	originalOwner := source.cursor.Load().owner.Version()
	_, _, verified := anchorLocalTestRevoke(t, s, proof)
	result, _, err := s.CommitAnchorDenial(verified)
	if err != nil || result.Outcome() != AnchorCommitted {
		t.Fatal(err)
	}
	if !before.Same(imageOf(t, s).Generation()) || source.cursor.Load().owner.Version() != originalOwner {
		t.Fatal("deny fixture invented canonical progress")
	}
	if input.Current() || input.CacheDomain() != oldDomain {
		t.Fatal("local-only deny missed by source predicate/restamped cache identity")
	}
	if anchorCacheDomain(p.Projection().DomainID(), p.Projection().OwnerID(), source.cursor.Load().local.digest, input.cell.cacheNonce) == oldDomain {
		t.Fatal("cache domain omitted full local postimage")
	}
}

func TestAnchorPublicationCacheDomainReopenSeparatesEqualDurableBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	s := openAt(t, path, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	anchorTestManaged(t, s, dormant)
	original := anchorTestContext(t.Context(), t, s)
	_, first := anchorPublicationTestCapture(original, t, s)
	beforeLocal := anchorLocalTestRead(t, s)
	beforeOwner := first.Projection().OwnerID()
	firstDomain := first.Input().CacheDomain()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openAt(t, path, internalOptions)
	nextOriginal := anchorTestContext(t.Context(), t, reopened)
	if _, err := reopened.BindAnchorLifecycle(nextOriginal); err != nil {
		t.Fatal(err)
	}
	_, second := anchorPublicationTestCapture(nextOriginal, t, reopened)
	if second.Projection().OwnerID() != beforeOwner || !sameAnchorLocal(beforeLocal, anchorLocalTestRead(t, reopened)) {
		t.Fatal("reopen fixture changed accepted/local bytes")
	}
	if first.Input().Current() || !second.Input().Current() || first.Input().CacheDomain() != firstDomain || second.Input().CacheDomain() == firstDomain {
		t.Fatal("old Store/cache domain revived after reopen")
	}
}
