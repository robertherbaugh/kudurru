package persistence_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

func TestSQLiteNoopInitializationStillChecksOriginalLifetime(t *testing.T) {
	s, _ := seedStore(t)
	initialize(t, s)
	before := imageOf(t, s)
	commandCtx := carry(t.Context(), t, s)
	ctx, cancel := context.WithCancel(commandCtx)
	defer cancel()
	original, err := persistence.CaptureInitialization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(commandCtx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := initializeOwner(ctx, tx, original); err != nil {
			return err
		}
		cancel() // callback context remains live; ORIGINAL startup work stopped.
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("unchanged owner bypassed original final check", err)
	}
	unchanged(t, s, before)
}

func TestSQLiteFailedInitializationPreservesExactSentinel(t *testing.T) {
	s, _ := seedStore(t)
	before := imageOf(t, s)
	ctx := carry(t.Context(), t, s)
	original, err := persistence.CaptureInitialization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("fixture: original startup source refused")
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error { return refusal }); err != nil {
			return err
		}
		_, err := initializeOwner(ctx, tx, original)
		return err
	})
	if !errors.Is(err, refusal) {
		t.Fatal("refused startup committed", err)
	}
	unchanged(t, s, before)
	if !accepted.IsAwaitingOwner(imageOf(t, s).Payload.Runtime) {
		t.Fatal("rollback lost non-granting sentinel")
	}
}

func TestSQLiteOriginalWriterSameTermReplacementRefused(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, dormant)
	original := capture(ctx, t, dormant, clock)
	state, observation := proposal(t, s, key, clock, 3600)
	beforeAuthority := s.Authority()
	if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	if !beforeAuthority.Term.Same(s.Authority().Term) || beforeAuthority.Incarnation == s.Authority().Incarnation {
		t.Fatal("fixture did not replace same-term incarnation")
	}
	before := imageOf(t, s)
	called := false
	err := runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		called = true
		_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
		return err
	})
	if !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("Store admitted old incarnation", err)
	}
	// Separate adapter-only prewrite probe: a currently admitted outer command
	// cannot replace this original writer. It never reuses the consumed update.
	err = s.Command(carry(t.Context(), t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
		return err
	})
	if !errors.Is(err, persistence.ErrOriginal) {
		t.Fatal("adapter adopted successor writer", err)
	}
	unchanged(t, s, before)
}

func TestSQLiteOriginalTermReplacementRefused(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, dormant)
	original := capture(ctx, t, dormant, clock)
	state, observation := proposal(t, s, key, clock, 3600)
	next := originalTerm(ctx, t)
	next.Epoch++
	next.PrimaryID, next.PromotionID = "successor-primary", "successor-promotion"
	if _, err := s.ObserveAuthority(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, s)
	err := runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
		return err
	})
	if err == nil {
		t.Fatal("original term survived successor authority")
	}
	unchanged(t, s, before)
}

func originalTerm(ctx context.Context, t *testing.T) replication.Term {
	t.Helper()
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		t.Fatal("fixture lost original token")
	}
	return token.Term()
}

func TestSQLiteBootstrapCannotReseedManagedOrReplaceOriginalEvidence(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	ctx := carry(t.Context(), t, s)
	// These independent adapter-only prewrite probes deliberately supply bad
	// proof/phase inputs; no refused mounted operation is retried or rescued.
	original := capture(ctx, t, dormant, clock)
	_, observation := proposal(t, s, key, clock, 3600)
	otherState, _ := proposal(t, s, newSigner(t), clock, 3600)
	before := imageOf(t, s)
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, original, otherState, observation)
		return err
	})
	if !errors.Is(err, trustanchors.ErrStale) {
		t.Fatal("proof accepted different proposed seed", err)
	}
	unchanged(t, s, before)
	if _, _, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-operator-seed", DS: []string{key.key.ToDS(dns.SHA256).String()}}, signed(t, key, 0, false), clock.Sample()); !errors.Is(err, trustanchors.ErrExpired) {
		t.Fatal("zero TTL manufactured usable bootstrap", err)
	}
	managed := bootstrap(t, s, dormant, key, clock)
	managedOriginal := capture(ctx, t, managed, clock)
	reseed, reseedObservation := proposal(t, s, newSigner(t), clock, 3600)
	before = imageOf(t, s)
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Bootstrap(ctx, tx, managedOriginal, reseed, reseedObservation)
		return err
	})
	if !errors.Is(err, persistence.ErrPhase) {
		t.Fatal("managed history reset by reseed", err)
	}
	unchanged(t, s, before)
}

func TestSQLiteRestartAndFollowerImagePreserveTombstonesQuarantine(t *testing.T) {
	primary, primaryPath := seedStore(t)
	dormant := initialize(t, primary)
	key, clock := newSigner(t), newClock(t)
	managed := bootstrap(t, primary, dormant, key, clock)
	retired := advance(t, primary, managed, key, clock, true)
	state, err := retired.State()
	if err != nil {
		t.Fatal(err)
	}
	doc := state.Inspect()
	canonicalState, err := trustanchors.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Quarantined || len(doc.Keys) != 1 || doc.Keys[0].Status != trustanchors.Revoked {
		t.Fatal("actual self-revoke did not retain permanent deny/quarantine")
	}
	if _, err := primary.EnablePrimary(t.Context(), imageOf(t, primary).Generation()); err != nil {
		t.Fatal(err)
	}
	followerPath := filepath.Join(t.TempDir(), sqlite.FileName)
	follower := openStore(t, followerPath, replication.Follower, true)
	if err := follower.Enroll(t.Context(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	originalInstall := finite(t.Context(), t)
	bind(originalInstall, t, follower) // Read-only actual awaiting owner/carrier.
	installCtx, err := follower.CaptureAnchorInstall(originalInstall)
	if err != nil {
		t.Fatal(err)
	}
	a := follower.Authority() // Exact never-installed baseline, before acquisition.
	image, err := primary.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := follower.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed}); err != nil {
		t.Fatal(err)
	}
	if _, err := follower.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower gained writer", err)
	}
	called := false
	if err := follower.Command(t.Context(), follower.Authority().Term, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrReadOnly) || called {
		t.Fatal("follower advanced independently", err)
	}
	for _, s := range []*sqlite.Store{primary, follower} {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	primary = openStore(t, primaryPath, replication.Primary, true)
	follower = openStore(t, followerPath, replication.Follower, true)
	for _, s := range []*sqlite.Store{primary, follower} {
		original := finite(t.Context(), t)
		if s.Authority().Writable() {
			original = carry(original, t, s)
		}
		bind(original, t, s) // Actual Store.Open/restart restores its durable denial.
		i := imageOf(t, s)
		if !i.Generation().Same(image.Generation()) || !reflect.DeepEqual(i.Payload.Runtime, image.Payload.Runtime) {
			t.Fatal("restart/install changed canonical runtime rows or source generation")
		}
		restored, err := accepted.DecodeRows(i.Payload.Runtime, i.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		if restored.Version() != retired.Version() {
			t.Fatal("restart/install advanced or discarded history")
		}
		state, err := restored.State()
		if err != nil {
			t.Fatal(err)
		}
		got := state.Inspect()
		encoded, err := trustanchors.Encode(state)
		if err != nil || !bytes.Equal(encoded, canonicalState) || !reflect.DeepEqual(got, doc) {
			t.Fatal("restart/install changed exact pure state, permanent tombstones or elapsed checkpoint", err)
		}
		if !got.Quarantined || got.Keys[0].Status != trustanchors.Revoked || got.Checkpoint != doc.Checkpoint {
			t.Fatal("restart/install invented progress or removed quarantine")
		}
	}
	before := imageOf(t, primary)
	if initialize(t, primary).Version() != retired.Version() {
		t.Fatal("normal startup reset managed history")
	}
	unchanged(t, primary, before)
}

func TestSQLitePrefixBoundsBeforeDecodeAndNoRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	s := openStore(t, path, replication.Standalone, false)
	corruptClosed(t.Context(), t, s, path, func(ctx context.Context, tx *sql.Tx) error {
		for i := 0; i < accepted.MaxRows+1; i++ {
			key := accepted.Prefix + "unknown/" + string(rune('A'+i))
			if _, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, key, `{}`); err != nil {
				return err
			}
		}
		return nil
	})
	s = openStore(t, path, replication.Standalone, true)
	before := imageOf(t, s)
	entered := false
	err := s.Command(carry(t.Context(), t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		entered = true
		_, err := persistence.Load(ctx, tx)
		return err
	})
	if !entered {
		t.Fatal("oversized fixture failed before intended adapter bounds check", err)
	}
	if !errors.Is(err, replication.ErrTooLarge) {
		t.Fatal("prefix allocation not refused before decode", err)
	}
	unchanged(t, s, before)
}
