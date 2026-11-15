package persistence_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

type ownerStager func(context.Context, *sqlite.CommandTx) (persistence.Staged, error)

func prepareOwnerStage(t *testing.T, mode string) (*sqlite.Store, context.Context, ownerStager, *sqlite.AnchorUpdate) {
	t.Helper()
	s, _ := seedStore(t)
	if mode == "initialize" {
		ctx := carry(t.Context(), t, s)
		original, err := persistence.CaptureInitialization(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s, ctx, func(ctx context.Context, tx *sqlite.CommandTx) (persistence.Staged, error) {
			return initializeOwner(ctx, tx, original)
		}, nil
	}
	dormant := initialize(t, s)
	if mode == "unchanged dormant" {
		ctx := carry(t.Context(), t, s)
		original, err := persistence.CaptureInitialization(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s, ctx, func(ctx context.Context, tx *sqlite.CommandTx) (persistence.Staged, error) {
			return initializeOwner(ctx, tx, original)
		}, nil
	}
	key, clock := newSigner(t), newClock(t)
	if mode == "bootstrap" {
		ctx := carry(t.Context(), t, s)
		update := captureUpdate(ctx, t, s, dormant)
		original := capture(ctx, t, dormant, clock)
		state, observation := proposal(t, s, key, clock, 3600)
		return s, ctx, func(ctx context.Context, tx *sqlite.CommandTx) (persistence.Staged, error) {
			return persistence.Bootstrap(ctx, tx, original, state, observation)
		}, update
	}
	managed := bootstrap(t, s, dormant, key, clock)
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, managed)
	if mode == "unchanged managed" {
		original, err := persistence.CaptureInitialization(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return s, ctx, func(ctx context.Context, tx *sqlite.CommandTx) (persistence.Staged, error) {
			return initializeOwner(ctx, tx, original)
		}, update
	}
	if mode != "advance" {
		t.Fatal("unknown controlled fixture operation")
	}
	original := capture(ctx, t, managed, clock)
	state, err := managed.State()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, signed(t, key, 3600, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	return s, ctx, func(ctx context.Context, tx *sqlite.CommandTx) (persistence.Staged, error) {
		return persistence.Advance(ctx, tx, original, observation)
	}, update
}

func ownerRootRow(ctx context.Context, tx *sqlite.CommandTx) (replication.Entry, error) {
	data, err := tx.GetSection(ctx, accepted.Section, accepted.RootKey)
	return replication.Entry{Key: accepted.RootKey, Data: data}, err
}

func replaceOwnerHistory(ctx context.Context, tx *sqlite.CommandTx, row replication.Entry) error {
	var root accepted.Root
	if err := json.Unmarshal(row.Data, &root); err != nil {
		return err
	}
	history := strings.Repeat("f", 32)
	if root.HistoryID == history {
		history = strings.Repeat("e", 32)
	}
	root.HistoryID = history
	data, err := json.Marshal(root)
	if err != nil {
		return err
	}
	return tx.PutSection(ctx, accepted.Section, replication.Entry{Key: row.Key, Data: data})
}

func orphanOwnerChunk() (replication.Entry, error) {
	data := []byte("unreferenced controlled chunk")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	encoded, err := json.Marshal(accepted.Chunk{FormatVersion: accepted.FormatVersion, Digest: digest, Data: data})
	return replication.Entry{Key: accepted.ChunkPrefix + digest, Data: encoded}, err
}

func TestSQLiteOwnerPostimageFenceRejectsLaterMutationAndABA(t *testing.T) {
	for _, operation := range []string{"initialize", "unchanged dormant", "unchanged managed", "bootstrap", "advance"} {
		t.Run(operation, func(t *testing.T) {
			for _, tail := range []string{"delete root", "replace history", "orphan chunk", "put ABA", "delete ABA", "chunk insert delete ABA", "second registration"} {
				t.Run(tail, func(t *testing.T) {
					s, commandCtx, stage, update := prepareOwnerStage(t, operation)
					before := imageOf(t, s)
					staged := false
					err := runOwnerCommand(commandCtx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
						if _, err := stage(ctx, tx); err != nil {
							return err
						}
						staged = true
						// This independent receipt must roll back with the protected owner.
						if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "adapter/receipt", Data: json.RawMessage(`{"staged":true}`)}); err != nil {
							return err
						}
						root, err := ownerRootRow(ctx, tx)
						if err != nil {
							return err
						}
						switch tail {
						case "delete root":
							return tx.DeleteSection(ctx, accepted.Section, accepted.RootKey)
						case "replace history":
							return replaceOwnerHistory(ctx, tx, root)
						case "orphan chunk", "chunk insert delete ABA":
							chunk, err := orphanOwnerChunk()
							if err != nil {
								return err
							}
							if err := tx.PutSection(ctx, accepted.Section, chunk); err != nil {
								return err
							}
							if tail == "chunk insert delete ABA" {
								return tx.DeleteSection(ctx, accepted.Section, chunk.Key)
							}
							return nil
						case "put ABA":
							if err := replaceOwnerHistory(ctx, tx, root); err != nil {
								return err
							}
						default:
							if err := tx.DeleteSection(ctx, accepted.Section, accepted.RootKey); err != nil {
								return err
							}
						}
						if err := tx.PutSection(ctx, accepted.Section, root); err != nil {
							return err
						}
						if tail == "second registration" {
							return tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
						}
						return nil
					})
					if !staged || !errors.Is(err, replication.ErrCAS) {
						t.Fatal("late owner mutation bypassed staged postimage fence", err)
					}
					unchanged(t, s, before)
				})
			}
		})
	}
}

func TestSQLiteOwnerPostimageFencePermitsForeignCompositionAndTrueNoops(t *testing.T) {
	for _, operation := range []string{"initialize", "unchanged dormant", "unchanged managed", "bootstrap", "advance"} {
		t.Run(operation, func(t *testing.T) {
			for _, tail := range []string{"identical root put", "absent owned delete", "foreign runtime prefix", "same prefix other section"} {
				t.Run(tail, func(t *testing.T) {
					s, commandCtx, stage, update := prepareOwnerStage(t, operation)
					var staged persistence.Staged
					err := runOwnerCommand(commandCtx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
						var err error
						staged, err = stage(ctx, tx)
						if err != nil {
							return err
						}
						switch tail {
						case "identical root put":
							root, err := ownerRootRow(ctx, tx)
							if err != nil {
								return err
							}
							return tx.PutSection(ctx, accepted.Section, root)
						case "absent owned delete":
							return tx.DeleteSection(ctx, accepted.Section, accepted.ChunkPrefix+strings.Repeat("0", 64))
						case "foreign runtime prefix":
							return tx.PutSection(ctx, accepted.Section, replication.Entry{Key: "independent/head", Data: json.RawMessage(`{"committed":true}`)})
						case "same prefix other section":
							return tx.PutSection(ctx, "dedup", replication.Entry{Key: accepted.RootKey, Data: json.RawMessage(`{"committed":true}`)})
						}
						return nil
					})
					if err != nil {
						t.Fatal("independent composition or genuine no-op refused", err)
					}
					if load(t, s).Version() != staged.ProposedVersion {
						t.Fatal("composition changed intended accepted owner")
					}
				})
			}
		})
	}
}

func TestSQLiteOwnerFenceRegistrationFailureRollsBackAndPoisonsIgnoredError(t *testing.T) {
	for _, operation := range []string{"initialize", "unchanged dormant", "unchanged managed", "bootstrap", "advance"} {
		t.Run(operation, func(t *testing.T) {
			s, commandCtx, stage, update := prepareOwnerStage(t, operation)
			before := imageOf(t, s)
			var adapterErr error
			err := runOwnerCommand(commandCtx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
				// Leave exactly one check for the original proof/context hook. The
				// required postimage fence cannot fit after that, even on init no-op.
				for range sqlite.MaxCommitChecks - 1 {
					if err := tx.RequireCommitCheck(func(context.Context) error { return nil }); err != nil {
						return err
					}
				}
				if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "pre-owner/receipt", Data: json.RawMessage(`{"staged":true}`)}); err != nil {
					return err
				}
				_, adapterErr = stage(ctx, tx)
				return nil // Deliberately ignore registration refusal to prove poison.
			})
			if !errors.Is(adapterErr, replication.ErrInvalid) || !errors.Is(err, replication.ErrInvalid) {
				t.Fatal("ignored adapter fence failure committed", err)
			}
			unchanged(t, s, before)
		})
	}
}
