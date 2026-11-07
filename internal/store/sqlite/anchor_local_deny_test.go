package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

func anchorLocalTestFinite(ctx context.Context, t *testing.T) context.Context {
	t.Helper()
	original, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	return original
}

func anchorLocalTestFollower(t *testing.T, primary *Store) (*Store, *AnchorSource) {
	t.Helper()
	follower := mirror(t, primary)
	anchorLocalTestAwaiting(t, follower)
	source, err := follower.BindAnchorLifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return follower, source
}

func anchorLocalTestInstall(ctx context.Context, t *testing.T, follower, primary *Store) error {
	t.Helper()
	captured, err := follower.CaptureAnchorInstall(ctx)
	if err != nil {
		return err
	}
	cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
	image, err := primary.ReadGeneration(captured)
	if err != nil {
		return err
	}
	validated, err := follower.ValidateImage(captured, *image)
	if err != nil {
		return err
	}
	_, err = follower.InstallReplica(captured, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed})
	return err
}

func anchorLocalTestRevoke(t *testing.T, s *Store, proof anchorTestProof) (*AnchorDenial, *AnchorUpdate, *AnchorVerifiedDenial) {
	t.Helper()
	denial, update, err := s.CaptureAnchorRevocation(anchorTestContext(t.Context(), t, s), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	return denial, update, verified
}

func anchorLocalTestAdvance(ctx context.Context, tx *CommandTx, observation trustanchors.Observation, proof anchorTestProof) error {
	state, err := trustanchors.Advance(proof.state, observation, proof.clock.Sample())
	if err != nil {
		return err
	}
	next, err := accepted.ProposeManaged(proof.next, state, state.Inspect().Checkpoint.Wall, observation.ValidUntil())
	if err != nil {
		return err
	}
	return anchorTestStage(ctx, tx, next)
}

func TestAnchorLocalFollowerDenyRequiresNoPrimaryAndChangesNoCanonicalState(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	original := source.cursor.Load()
	before := imageOf(t, follower).Generation()
	a := anchorAuthority(follower.Authority())
	if _, err := follower.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower gained primary", err)
	}
	capture, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := capture.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	receipt, own, err := follower.CommitAnchorDenial(verified)
	if err != nil || receipt.Outcome() != AnchorCommitted || own == nil {
		t.Fatal("actual follower denial failed", err)
	}
	after := source.cursor.Load()
	if after.owner.Version() != original.owner.Version() || !before.Same(imageOf(t, follower).Generation()) || a != anchorAuthority(follower.Authority()) {
		t.Fatal("deny-only lane changed canonical history or authority")
	}
	if after.local.record.RetiredThrough != proof.next.Version().Generation || len(after.local.record.Denied) != 1 {
		t.Fatal("inclusive durable denial missing")
	}
	if !sameAnchorLocal(after.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("denial publication not exact committed local bytes")
	}
	state, _ := after.owner.State()
	old, _ := original.owner.State()
	if state.Inspect().Checkpoint != old.Inspect().Checkpoint {
		t.Fatal("follower invented timer progress")
	}
}

func TestAnchorLocalTwoPhaseUsesOnlyOwnConfirmedPostimage(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	_, update, verified := anchorLocalTestRevoke(t, s, proof)
	_, receipt, err := s.CommitAnchorDenial(verified)
	if err != nil || receipt == nil {
		t.Fatal(err)
	}
	phase1 := source.cursor.Load()
	first := phase1.local.record.Denied[0]
	outcome, err := s.AnchorCommandAfterDenial(update, receipt, func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	})
	if err != nil || outcome.Outcome() != AnchorCommitted {
		t.Fatal("own exact effect was not composable", err)
	}
	final := source.cursor.Load()
	state, err := final.owner.State()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Inspect().Quarantined || state.Inspect().Keys[0].Status != trustanchors.Revoked {
		t.Fatal("canonical self-revoke lost quarantine/tombstone")
	}
	if final.owner.Version().Generation != proof.next.Version().Generation+1 || final.local.record.Denied[0] != first {
		t.Fatal("phase2 changed first denial receipt or original version")
	}
	if !sameAnchorLocal(final.local, anchorLocalTestRead(t, s)) {
		t.Fatal("phase2 source/local mismatch")
	}
}

func TestAnchorLocalReceiptCopiedForeignMismatchedAndReused(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	_, update, verified := anchorLocalTestRevoke(t, s, proof)
	_, receipt, err := s.CommitAnchorDenial(verified)
	if err != nil {
		t.Fatal(err)
	}
	copyReceipt := *receipt
	other := openTemp(t)
	called := false
	fn := func(ctx context.Context, tx *CommandTx, observation trustanchors.Observation) error {
		called = true
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	}
	if _, err := other.AnchorCommandAfterDenial(update, &copyReceipt, fn); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("foreign Store consumed receipt", err)
	}
	otherDenial, otherUpdate, err := s.CaptureAnchorRevocation(anchorTestContext(t.Context(), t, s), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	_ = otherDenial
	if _, err := s.AnchorCommandAfterDenial(otherUpdate, receipt, fn); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("foreign operation consumed own receipt", err)
	}
	if _, err := s.AnchorCommand(update, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrInvalid) || called {
		t.Fatal("paired update bypassed own-effect receipt", err)
	}
	if _, err := s.AnchorCommandAfterDenial(update, receipt, fn); err != nil || !called {
		t.Fatal("owner lost original receipt after foreign refusals", err)
	}
	called = false
	if _, err := s.AnchorCommandAfterDenial(update, &copyReceipt, fn); !errors.Is(err, retirement.ErrUsed) || called {
		t.Fatal("copied receipt replayed", err)
	}
	if _, _, err := s.CommitAnchorDenial(verified); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("verified input replayed", err)
	}
}

func TestAnchorLocalPhase2RefusesInterposedWriterDenialAndCanonicalChange(t *testing.T) {
	for _, kind := range []string{"writer-incarnation", "other-denial", "canonical-writer"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			_, update, verified := anchorLocalTestRevoke(t, s, proof)
			_, own, err := s.CommitAnchorDenial(verified)
			if err != nil {
				t.Fatal(err)
			}
			durable := anchorLocalTestRead(t, s)
			switch kind {
			case "writer-incarnation":
				if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
					t.Fatal(err)
				}
			case "other-denial":
				// A second independent known-deny transaction can confirm the same durable
				// bytes, but its distinct immutable cursor must not be adopted by phase2.
				denial, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
				if err != nil {
					t.Fatal(err)
				}
				p, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.CommitAnchorDenial(p); err != nil {
					t.Fatal(err)
				}
			case "canonical-writer":
				// Independent canonical writer does NOT adopt the first operation's proof.
				later, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), proof.next.Version())
				if err != nil {
					t.Fatal(err)
				}
				observation, err := trustanchors.Verify(proof.state, anchorTestSigned(t, proof.key, proof.signer, true), proof.clock.Sample())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.AnchorCommand(later, func(ctx context.Context, tx *CommandTx) error {
					return anchorLocalTestAdvance(ctx, tx, observation, proof)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := source.cursor.Load()
			called := false
			receipt, err := s.AnchorCommandAfterDenial(update, own, func(context.Context, *CommandTx, trustanchors.Observation) error { called = true; return nil })
			if err == nil || called || receipt.Outcome() != AnchorRolledBack {
				t.Fatal("phase2 adopted interposed source/writer", err)
			}
			if source.cursor.Load() != before {
				t.Fatal("refused phase2 changed source")
			}
			if source.cursor.Load().local.record.RetiredThrough < durable.record.RetiredThrough {
				t.Fatal("phase1 durable floor lost")
			}
		})
	}
}

func TestAnchorLocalCancellationAfterPhase1NeverReplaysOrLosesDeny(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	original, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	denial, update, err := s.CaptureAnchorRevocation(original, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	_, own, err := s.CommitAnchorDenial(verified)
	if err != nil {
		t.Fatal(err)
	}
	durable := source.cursor.Load()
	cancel()
	called := false
	result, err := s.AnchorCommandAfterDenial(update, own, func(context.Context, *CommandTx, trustanchors.Observation) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called || result.Outcome() != AnchorRolledBack {
		t.Fatal("cancellation replaced original authority", err)
	}
	if source.cursor.Load() != durable || !sameAnchorLocal(durable.local, anchorLocalTestRead(t, s)) {
		t.Fatal("canceled phase2 rolled back actual phase1")
	}
	if _, err := s.AnchorCommandAfterDenial(update, own, func(context.Context, *CommandTx, trustanchors.Observation) error { return nil }); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("canceled receipt reused", err)
	}
}

func TestAnchorLocalFailedUnknownPhase1ClosesWithoutReceipt(t *testing.T) {
	for _, kind := range []string{"rollback", "commit-unknown"} {
		t.Run(kind, func(t *testing.T) {
			s, source, proof := anchorLocalTestManaged(t)
			_, update, verified := anchorLocalTestRevoke(t, s, proof)
			before := imageOf(t, s).Generation()
			local := anchorLocalTestRead(t, s)
			if kind == "rollback" {
				if _, err := s.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER anchor_deny_refuse BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture denial refused'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, statement := range []string{
					`CREATE TEMP TABLE anchor_deny_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE anchor_deny_child(parent INTEGER REFERENCES anchor_deny_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER anchor_deny_unknown AFTER UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN INSERT INTO anchor_deny_child(parent) VALUES (1); END`,
				} {
					if _, err := s.w.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			outcome, own, err := s.CommitAnchorDenial(verified)
			expected := AnchorRolledBack
			if kind == "commit-unknown" {
				expected = AnchorUnknown
			}
			if err == nil || own != nil || outcome.Outcome() != expected || !source.closed.Load() {
				t.Fatal("unconfirmed denial reopened/credited durability", err)
			}
			if !before.Same(imageOf(t, s).Generation()) || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
				t.Fatal("failed actual denial escaped SQLite rollback")
			}
			if _, err := s.AnchorCommandAfterDenial(update, own, func(context.Context, *CommandTx, trustanchors.Observation) error { return nil }); !errors.Is(err, replication.ErrInvalid) {
				t.Fatal("failed phase1 minted receipt", err)
			}
		})
	}
}

func TestAnchorLocalConfirmedDenyResolutionIgnoresPostCommitCancellation(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	original, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	d, err := s.CaptureAnchorDenial(original, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	c := d.cell
	operation := &anchorOperation{source: source, before: c.cursor, authorityCut: c.authority, sql: anchorSQL{outcome: AnchorRolledBack}}
	var after *anchorCursor
	// Compose the actual outer mode hold while retaining this private fixture's
	// exact post-Commit, pre-Resolve cancellation boundary and original R.
	err = s.withAnchorMode(c.mode, operation, func() error {
		return c.cursor.gate.WithWrite(c.attempt, func(scope *retirement.WriteScope) error {
			w, err := scope.PrepareWithdrawal()
			if err != nil {
				return err
			}
			if err := s.writeAnchorDenial(c, p.cell, operation, w, &after); err != nil {
				return err
			}
			if operation.sql.outcome != AnchorCommitted {
				t.Fatal("fixture did not reach real commit")
			}
			cancel() // exact boundary AFTER SQL Commit, BEFORE original plan Resolve
			if err := w.Resolve(retirement.ConfirmedDurableDeny); err != nil {
				return err
			}
			if !source.cursor.CompareAndSwap(c.cursor, after) {
				return accepted.ErrCAS
			}
			return nil
		})
	})
	if err != nil || source.cursor.Load() != after || !sameAnchorLocal(after.local, anchorLocalTestRead(t, s)) {
		t.Fatal("completed deny became rollback after cancellation", err)
	}
}

func TestAnchorLocalFirstInstallPrivateCandidatesRaceAndExactReceiver(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	image := imageOf(t, primary)
	contexts := make([]context.Context, 2)
	for i := range contexts {
		var err error
		contexts[i], err = follower.CaptureAnchorInstall(anchorLocalTestFinite(t.Context(), t))
		if err != nil {
			t.Fatal(err)
		}
		cell := contexts[i].Value(anchorInstallKey{}).(*anchorInstall)
		if !cell.installed.Same(replication.Generation{}) {
			t.Fatal("first capture invented installed baseline")
		}
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, ctx := range contexts {
		go func(ctx context.Context) {
			ready.Done()
			select {
			case <-start:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
			_, err := follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: replication.Generation{}})
			done <- err
		}(ctx)
	}
	ready.Wait()
	if !source.cursor.Load().awaiting || source.cursor.Load().gate != nil {
		t.Fatal("proposal registered first-bind candidate")
	}
	close(start)
	passes, refused := 0, 0
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				passes++
			} else if errors.Is(err, accepted.ErrCAS) {
				refused++
			} else {
				t.Fatal("unexpected competing-install category", err)
			}
		case <-contexts[0].Done():
			t.Fatal("first-install race did not join")
		}
	}
	if passes != 1 || refused != 1 {
		t.Fatal("first-install CAS did not choose exactly one candidate")
	}
	current := source.cursor.Load()
	local := anchorLocalTestRead(t, follower)
	if current.owner.Version() != proof.next.Version() || current.gate == nil || current.local.raw != local.raw {
		t.Fatal("first actual install did not publish exact resolved candidate")
	}
	if local.record.Member != follower.Authority().MemberID || local.record.Carrier == anchorLocalTestRead(t, primary).record.Carrier {
		t.Fatal("receiver copied sender local identity")
	}
}

func TestAnchorLocalInstallRetainsDenyAndRequiresTombstoneCoverage(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	denial, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := follower.CommitAnchorDenial(verified); err != nil {
		t.Fatal(err)
	}
	durable := anchorLocalTestRead(t, follower)
	// A NEW independently authenticated primary observation supplies canonical
	// tombstones. The follower neither advances pure state nor merges latest data.
	_, update, original := anchorLocalTestRevoke(t, primary, proof)
	_, own, err := primary.CommitAnchorDenial(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.AnchorCommandAfterDenial(update, own, func(ctx context.Context, tx *CommandTx, o trustanchors.Observation) error {
		return anchorLocalTestAdvance(ctx, tx, o, proof)
	}); err != nil {
		t.Fatal(err)
	}
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	current := source.cursor.Load()
	state, err := current.owner.State()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Inspect().Quarantined || state.Inspect().Keys[0].Status != trustanchors.Revoked || current.local.record.RetiredThrough < durable.record.RetiredThrough {
		t.Fatal("canonical install dropped durable denial/quarantine")
	}
	if !slices.Equal(current.local.record.Denied, durable.record.Denied) {
		t.Fatal("install replaced first local receipts")
	}
	// Byte-exact no-op must retain local denial and never grant/credit progress.
	before := current
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	if source.cursor.Load() != before {
		t.Fatal("same-image no-op reset local cursor")
	}
}

func TestAnchorLocalInstallCannotOmitPendingDeny(t *testing.T) {
	primary, _, proof := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	if err := anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary); err != nil {
		t.Fatal(err)
	}
	denial, err := follower.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := denial.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := follower.CommitAnchorDenial(verified); err != nil {
		t.Fatal(err)
	}
	before := source.cursor.Load()
	installed := imageOf(t, follower).Generation()
	update, err := primary.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, primary), proof.next.Version())
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(proof.state, anchorTestSigned(t, proof.key, proof.signer, false), proof.clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
		return anchorLocalTestAdvance(ctx, tx, observation, proof)
	}); err != nil {
		t.Fatal(err)
	}
	err = anchorLocalTestInstall(anchorLocalTestFinite(t.Context(), t), t, follower, primary)
	if !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("newer image erased pending durable denial", err)
	}
	if source.cursor.Load() != before || !installed.Same(imageOf(t, follower).Generation()) || !sameAnchorLocal(before.local, anchorLocalTestRead(t, follower)) {
		t.Fatal("refused image replaced owner/local cut")
	}
}

func TestAnchorLocalOriginalFiniteDeadlineAndSingleInput(t *testing.T) {
	s, _, proof := anchorLocalTestManaged(t)
	long, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if _, err := s.CaptureAnchorDenial(long, proof.next.Version(), proof.clock); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long original denial admitted", err)
	}
	if _, _, err := s.CaptureAnchorRevocation(long, proof.next.Version(), proof.clock); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("follower context manufactured original writer", err)
	}
	writer, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CaptureAnchorRevocation(WithWriterToken(long, writer), proof.next.Version(), proof.clock); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long original two-phase acquisition admitted", err)
	}
	if _, err := s.CaptureAnchorInstall(long); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long original install acquisition admitted", err)
	}
	d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Verify(nil); err == nil {
		t.Fatal("nil received input authenticated")
	}
	if _, err := d.Verify(anchorTestSigned(t, proof.key, proof.signer, true)); !errors.Is(err, retirement.ErrUsed) {
		t.Fatal("failed proof acquired a second input", err)
	}
}

func TestAnchorLocalFirstInstallCommitNotVisibleUntilPlanResolution(t *testing.T) {
	primary, _, _ := anchorLocalTestManaged(t)
	follower, source := anchorLocalTestFollower(t, primary)
	original, cancel := context.WithCancel(anchorLocalTestFinite(t.Context(), t))
	defer cancel()
	captured, err := follower.CaptureAnchorInstall(original)
	if err != nil {
		t.Fatal(err)
	}
	cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
	image := imageOf(t, primary)
	root, err := decodeAnchorCursor(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := root.owner.Inspect()
	candidate, err := retirement.New(retirement.History{ClusterID: metadata.ClusterID, HistoryID: metadata.HistoryID}, [32]byte{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := candidate.CaptureWrite(original, retirement.Mark{})
	if err != nil {
		t.Fatal(err)
	}
	operation := &anchorOperation{source: source, before: cell.cursor, installed: cell.installed, authorityCut: cell.authority, originalLife: &cell.life, installGate: candidate, sql: anchorSQL{outcome: AnchorRolledBack}}
	err = candidate.WithWrite(attempt, func(scope *retirement.WriteScope) error {
		change, err := scope.BeginChange()
		if err != nil {
			return err
		}
		operation.scope, operation.change = scope, change
		if _, err := follower.installReplica(original, *image, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed}, operation); err != nil {
			return err
		}
		if operation.sql.outcome != AnchorCommitted {
			t.Fatal("first install did not reach real SQL commit")
		}
		if source.cursor.Load() != cell.cursor || source.cursor.Load().gate != nil {
			t.Fatal("committed unresolved candidate became visible")
		}
		cancel()
		return operation.resolve()
	})
	if err != nil || source.cursor.Load() != operation.target || source.cursor.Load().gate != candidate {
		t.Fatal("completed first install lost exact resolved candidate", err)
	}
	if !sameAnchorLocal(source.cursor.Load().local, anchorLocalTestRead(t, follower)) {
		t.Fatal("first install published noncommitted local bytes")
	}
}

func TestAnchorLocalRestoreLoadsInclusiveFloorAndTerminalDenial(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := d.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitAnchorDenial(verified); err != nil {
		t.Fatal(err)
	}
	rows, err := loadAnchorRows(t.Context(), s.r)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := decodeAnchorCursor(rows, s.Authority().Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	restored.local = anchorLocalTestRead(t, s)
	if err := checkAnchorLocal(restored.local, restored, s.Authority().MemberID); err != nil {
		t.Fatal(err)
	}
	ctx := anchorLocalTestFinite(t.Context(), t)
	gate, err := restoreAnchorGate(ctx, restored)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := gate.CaptureWrite(ctx, restored.mark)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := [32]byte{1}
	err = gate.WithWrite(attempt, func(scope *retirement.WriteScope) error {
		_, err := scope.PrepareBinding(retirement.Frame{Mark: restored.mark, OwnerID: ownerID})
		return err
	})
	if !errors.Is(err, retirement.ErrRetired) {
		t.Fatal("restored pending denial registered an old response", err)
	}
	// This is actual committed-cut reconstruction only. Projection bytes and this
	// test's mechanical frame are never exposed as a runtime serving capability.
	if source.cursor.Load().owner.Version() != restored.owner.Version() {
		t.Fatal("restore fixture changed accepted history")
	}
}

func TestAnchorLocalNotificationsCoalesceWithoutGrantAuthority(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	for {
		select {
		case <-source.Changes():
			continue
		default:
			goto drained
		}
	}
drained:
	d, err := s.CaptureAnchorDenial(anchorLocalTestFinite(t.Context(), t), proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Verify(anchorTestSigned(t, proof.key, proof.signer, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CommitAnchorDenial(p); err != nil {
		t.Fatal(err)
	}
	source.notify()
	source.notify() // never allocate per-event queues or block
	select {
	case <-source.Changes():
	default:
		t.Fatal("confirmed local-only effect omitted readiness wakeup")
	}
	select {
	case <-source.Changes():
		t.Fatal("wakeups were not coalesced")
	default:
	}
	if source.cursor.Load().local.record.RetiredThrough < proof.next.Version().Generation {
		t.Fatal("notification replaced actual denial")
	}
}

func TestAnchorLocalHeldOriginalObservationExpiresWithoutRestamp(t *testing.T) {
	s, source, proof := anchorLocalTestManaged(t)
	original := anchorLocalTestFinite(t.Context(), t)
	denial, err := s.CaptureAnchorDenial(original, proof.next.Version(), proof.clock)
	if err != nil {
		t.Fatal(err)
	}
	message := anchorTestSigned(t, proof.key, proof.signer, true)
	for _, rr := range message.Answer {
		rr.Header().Ttl = 1
	}
	verified, err := denial.Verify(message)
	if err != nil {
		t.Fatal(err)
	}
	local := anchorLocalTestRead(t, s)
	// Wait for the ORIGINAL detached received-TTL bound, not a replacement
	// observation/clock or an enlarged caller lifetime. No network/request retry.
	timer := time.NewTimer(time.Until(verified.cell.observation.ValidUntil()))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-original.Done():
		t.Fatal("original caller expired before held-proof boundary")
	}
	result, receipt, err := s.CommitAnchorDenial(verified)
	if !errors.Is(err, trustanchors.ErrExpired) || receipt != nil || result.Outcome() != AnchorRolledBack {
		t.Fatal("held original TTL evidence was restamped", err)
	}
	if !source.closed.Load() || !sameAnchorLocal(local, anchorLocalTestRead(t, s)) {
		t.Fatal("expired known denial reopened or credited persistence")
	}
}

func TestAnchorLocalFirstInstallFailureNeverPublishesCandidate(t *testing.T) {
	for _, kind := range []string{"rollback", "commit-unknown"} {
		t.Run(kind, func(t *testing.T) {
			primary, _, _ := anchorLocalTestManaged(t)
			follower, source := anchorLocalTestFollower(t, primary)
			original := source.cursor.Load()
			local := anchorLocalTestRead(t, follower)
			authority := anchorAuthority(follower.Authority())
			ownerRows, err := loadAnchorRows(t.Context(), follower.r)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "rollback" {
				if _, err := follower.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER anchor_first_refuse BEFORE UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN SELECT RAISE(ABORT,'fixture first install refused'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				for _, statement := range []string{
					`CREATE TEMP TABLE anchor_first_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE anchor_first_child(parent INTEGER REFERENCES anchor_first_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER anchor_first_unknown AFTER UPDATE ON main.meta WHEN old.key='trustanchors-local/v1/state' BEGIN INSERT INTO anchor_first_child(parent) VALUES (1); END`,
				} {
					if _, err := follower.w.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			originalCtx := anchorLocalTestFinite(t.Context(), t)
			captured, err := follower.CaptureAnchorInstall(originalCtx)
			if err != nil {
				t.Fatal("fixture refused before original install capture", err)
			}
			cell := captured.Value(anchorInstallKey{}).(*anchorInstall)
			validated, err := follower.ValidateImage(captured, *imageOf(t, primary))
			if err != nil {
				t.Fatal("fixture image refused before injected SQL boundary", err)
			}
			image := validated.Image()
			// Match InstallReplica's receiver validation before its exact private
			// dispatch/body, retaining the original operation only for inspection.
			// No production hook, alternate admission, second attempt or replay.
			if _, err := follower.ValidateImage(captured, image); err != nil {
				t.Fatal("receiver revalidation refused before injected SQL boundary", err)
			}
			var operation *anchorOperation
			err = follower.withAnchorInstall(captured, image, func(ctx context.Context, current *anchorOperation) error {
				operation = current
				_, err := follower.installReplica(ctx, image, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: cell.installed}, current)
				return err
			})
			if err == nil {
				t.Fatal("failed first install accepted candidate")
			}
			if operation == nil || operation.scope == nil || operation.change == nil || operation.sql.tx == nil || operation.localTx != operation.sql.tx {
				t.Fatal("fixture never entered original scope and local SQL transaction")
			}
			var sqliteFailure interface{ Code() int }
			if !errors.As(err, &sqliteFailure) {
				t.Fatal("failure was not the injected SQLite constraint", err)
			}
			if kind == "rollback" {
				// SQLITE_CONSTRAINT_TRIGGER proves the targeted local UPDATE
				// reached the BEFORE trigger; a prior admission refusal cannot pass.
				if sqliteFailure.Code() != 1811 || operation.sql.attempted || operation.sql.committed || operation.sql.outcome != AnchorRolledBack || errors.Is(err, sql.ErrTxDone) {
					t.Fatal("targeted SQL abort did not confirm pre-commit rollback")
				}
			} else {
				// SQLITE_CONSTRAINT_FOREIGNKEY from the DEFERRABLE fixture plus
				// attempted Commit/unknown cleanup proves the actual commit boundary.
				if sqliteFailure.Code() != 787 || !operation.sql.attempted || operation.sql.committed || operation.sql.outcome != AnchorUnknown || !errors.Is(err, sql.ErrTxDone) {
					t.Fatal("deferred SQL failure did not retain unknown commit disposition")
				}
				if !operation.prepared || !operation.localChanged || operation.target == nil || operation.target.local == nil || sameAnchorLocal(local, operation.target.local) {
					t.Fatal("commit fixture never staged its exact owner and local postimage")
				}
			}
			afterRows, rowsErr := loadAnchorRows(t.Context(), follower.r)
			if rowsErr != nil || !slices.EqualFunc(ownerRows, afterRows, func(a, b replication.Entry) bool { return a.Key == b.Key && string(a.Data) == string(b.Data) }) || authority != anchorAuthority(follower.Authority()) {
				t.Fatal("failed first install changed canonical rows or authority", rowsErr)
			}
			if source.cursor.Load() != original || source.cursor.Load().gate != nil || !sameAnchorLocal(local, anchorLocalTestRead(t, follower)) {
				t.Fatal("failed/unknown first candidate became visible")
			}
			if kind == "commit-unknown" && !source.closed.Load() {
				t.Fatal("unknown first install left process admission open")
			}
		})
	}
}
