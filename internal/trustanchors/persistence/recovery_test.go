package persistence_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

func TestSQLiteRecoveryOriginalRollbackCommitAndPhysicalRestart(t *testing.T) {
	s, path := seedStore(t)
	dormant := initialize(t, s)
	first, replacement, clock := newSigner(t), newSigner(t), newClock(t)
	managed := bootstrap(t, s, dormant, first, clock)
	lost := advance(t, s, managed, first, clock, true)
	state, err := lost.State()
	if err != nil || !state.Inspect().Quarantined {
		t.Fatal("actual writer did not commit all-anchor loss", err)
	}
	before := imageOf(t, s)
	var committed accepted.Snapshot
	for _, rollback := range []bool{true, false} {
		ctx := carry(t.Context(), t, s)
		update := captureUpdate(ctx, t, s, lost)
		original := capture(ctx, t, lost, clock)
		proposed, proof, err := trustanchors.Recover(state, []string{replacement.key.ToDS(dns.SHA256).String()}, signed(t, replacement, 3600, false), clock.Sample())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := accepted.ProposeManaged(lost, proposed, proposed.Inspect().Checkpoint.Wall, proof.ValidUntil()); err == nil {
			t.Fatal("ordinary transition framed an operator recovery")
		}
		refusal := errors.New("original operator admission refused")
		staged := false
		completion, err := s.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if _, err := persistence.Recover(ctx, tx, original, proposed, proof); err != nil {
				return err
			}
			staged = true
			if rollback {
				return tx.RequireCommitCheck(func(context.Context) error { return refusal })
			}
			return nil
		})
		if !staged {
			t.Fatal("recovery fixture missed the real mutation target", err)
		}
		if rollback {
			if !errors.Is(err, refusal) || completion.Outcome() != sqlite.AnchorRolledBack {
				t.Fatal("original refusal lost rollback disposition", err)
			}
			unchanged(t, s, before)
			continue
		}
		if err != nil || completion.Outcome() != sqlite.AnchorCommitted {
			t.Fatal("original recovery did not commit", err)
		}
		committed = load(t, s)
		next, err := committed.State()
		if err != nil || next.Inspect().Quarantined || len(next.Inspect().Recoveries) != 1 {
			t.Fatal("committed recovery omitted trust provenance", err)
		}
		if !reflect.DeepEqual(state.Inspect().Keys[0], next.Inspect().Keys[indexKey(next.Inspect().Keys, state.Inspect().Keys[0].ID)]) {
			t.Fatal("committed recovery changed permanent revoked key")
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openStore(t, path, replication.Standalone, true)
	bind(carry(t.Context(), t, reopened), t, reopened)
	got := load(t, reopened)
	if got.Version() != committed.Version() {
		t.Fatal("physical restart changed accepted recovery")
	}
	gotState, err := got.State()
	if err != nil {
		t.Fatal(err)
	}
	current, err := committed.State()
	if err != nil || !reflect.DeepEqual(gotState.Inspect(), current.Inspect()) {
		t.Fatal("physical restart lost complete recovery history", err)
	}
	// Ordinary authenticated refresh after restart retains the recovery ledger.
	next := advance(t, reopened, got, replacement, newClock(t), false)
	nextState, err := next.State()
	if err != nil || !reflect.DeepEqual(nextState.Inspect().Recoveries, current.Inspect().Recoveries) {
		t.Fatal("ordinary restart refresh rewrote operator provenance", err)
	}
}

func indexKey(keys []trustanchors.Key, id string) int {
	for i, key := range keys {
		if key.ID == id {
			return i
		}
	}
	return 0 // equality still refuses a missing original key
}

func TestSQLiteRecoveryFollowerInstallHistoryRewriteAndRestart(t *testing.T) {
	primary, _ := seedStore(t)
	dormant := initialize(t, primary)
	first, replacement, clock := newSigner(t), newSigner(t), newClock(t)
	managed := bootstrap(t, primary, dormant, first, clock)
	lost := advance(t, primary, managed, first, clock, true)
	state, err := lost.State()
	if err != nil {
		t.Fatal(err)
	}
	ctx := carry(t.Context(), t, primary)
	update := captureUpdate(ctx, t, primary, lost)
	original := capture(ctx, t, lost, clock)
	proposed, proof, err := trustanchors.Recover(state, []string{replacement.key.ToDS(dns.SHA256).String()}, signed(t, replacement, 3600, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	completion, err := primary.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Recover(ctx, tx, original, proposed, proof)
		return err
	})
	if err != nil || completion.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("actual recovery did not commit", err)
	}
	recovered := load(t, primary)
	if _, err := primary.EnablePrimary(t.Context(), imageOf(t, primary).Generation()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	follower := openStore(t, path, replication.Follower, true)
	if err := follower.Enroll(t.Context(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	bind(finite(t.Context(), t), t, follower)
	install := func(rewrite bool) error {
		t.Helper()
		ctx, err := follower.CaptureAnchorInstall(finite(t.Context(), t))
		if err != nil {
			t.Fatal(err)
		}
		before := follower.Authority()
		image, err := primary.ReadGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if rewrite {
			rewriteRecoveryDigest(t, image)
		}
		validated, err := follower.ValidateImage(ctx, *image)
		if err != nil {
			t.Fatal("complete image validation masked installed-history comparison", err)
		}
		_, err = follower.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: before.Term, ExpectedInstalled: before.Installed})
		return err
	}
	if err := install(false); err != nil {
		t.Fatal("follower refused recovered history", err)
	}
	readFollower := func() accepted.Snapshot {
		t.Helper()
		image := imageOf(t, follower)
		owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		return owner
	}
	if readFollower().Version() != recovered.Version() {
		t.Fatal("follower changed recovered history")
	}
	continued := advance(t, primary, recovered, replacement, clock, false)
	before := imageOf(t, follower)
	if err := install(true); !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("installed recovery provenance was rewritten", err)
	}
	unchanged(t, follower, before)
	// This is a separate newly acquired valid generation, never a retry with the
	// refused attempt or its altered image.
	if err := install(false); err != nil {
		t.Fatal("ordinary retained-history successor refused", err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = openStore(t, path, replication.Follower, true)
	bind(finite(t.Context(), t), t, follower)
	got := readFollower()
	wantState, err := continued.State()
	if err != nil {
		t.Fatal(err)
	}
	gotState, err := got.State()
	if err != nil || got.Version() != continued.Version() || !reflect.DeepEqual(gotState.Inspect(), wantState.Inspect()) {
		t.Fatal("physical follower restart changed recovery or tombstones", err)
	}
	if _, err := follower.WriterToken(); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("recovery install granted a follower writer", err)
	}
}

// Build an independently well-formed owner/image with rewritten old provenance.
// Its complete validators must succeed so only the original installed cut can
// reject the rewrite. No production framing or validation rule is weakened.
func rewriteRecoveryDigest(t *testing.T, image *replication.Image) {
	t.Helper()
	owner, err := accepted.DecodeRows(image.Payload.Runtime, image.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := owner.State()
	if err != nil {
		t.Fatal(err)
	}
	doc := state.Inspect()
	doc.Recoveries[0].PreviousDigest = strings.Repeat("a", 64)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trustanchors.Decode(data); err != nil {
		t.Fatal("rewritten pure state is not independently valid", err)
	}
	digest := func(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
	root := owner.Inspect()
	root.Pure.Digest, root.StateBytes, root.Chunks = digest(data), len(data), nil
	var rows []replication.Entry
	for _, row := range image.Payload.Runtime {
		if !strings.HasPrefix(row.Key, accepted.Prefix) {
			rows = append(rows, row)
		}
	}
	add := func(key string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, replication.Entry{Key: key, Data: b})
	}
	for offset := 0; offset < len(data); offset += accepted.MaxChunkBytes {
		part := data[offset:min(offset+accepted.MaxChunkBytes, len(data))]
		hash := digest(part)
		root.Chunks = append(root.Chunks, accepted.ChunkRef{Digest: hash, Bytes: len(part)})
		add(accepted.ChunkPrefix+hash, accepted.Chunk{FormatVersion: accepted.FormatVersion, Digest: hash, Data: part})
	}
	add(accepted.RootKey, root)
	image.Payload.Runtime = rows
	if err := replication.Seal(image); err != nil {
		t.Fatal(err)
	}
}
