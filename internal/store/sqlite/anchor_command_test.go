package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

func TestAnchorCommandReservedPrefixPoisonWithoutValidators(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "registered"}[registered], func(t *testing.T) {
			for _, kind := range []string{"unknown-put", "unknown-delete", "root-delete", "managed-put", "unknown-ABA"} {
				t.Run(kind, func(t *testing.T) {
					s := openTemp(t, func(o *Options) {
						if registered {
							o.ValidateReplicaEntry = accepted.ValidateEntry
						}
					})
					dormant := anchorTestDormant(t, s)
					proof := anchorTestBootstrap(t, s, dormant)
					rows, err := accepted.EncodeRows(proof.next)
					if err != nil {
						t.Fatal(err)
					}
					var root replication.Entry
					for _, row := range rows {
						if row.Key == accepted.RootKey {
							root = row
						}
					}
					before := imageOf(t, s).Generation()
					ctx := anchorTestContext(t.Context(), t, s)
					err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
						if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "foreign/witness", Data: json.RawMessage(`{}`)}); err != nil {
							return err
						}
						switch kind {
						case "unknown-put":
							_ = tx.PutSection(ctx, accepted.Section, replication.Entry{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)})
						case "unknown-delete":
							_ = tx.DeleteSection(ctx, accepted.Section, "trustanchors/v99/root")
						case "root-delete":
							_ = tx.DeleteSection(ctx, accepted.Section, accepted.RootKey)
						case "managed-put":
							_ = tx.PutSection(ctx, accepted.Section, root)
						case "unknown-ABA":
							_ = tx.PutSection(ctx, accepted.Section, replication.Entry{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)})
							_ = tx.DeleteSection(ctx, accepted.Section, "trustanchors/v99/root")
						}
						return nil // Ignoring the original mutation refusal must poison the tx.
					})
					if !errors.Is(err, ErrAnchorUnavailable) {
						t.Fatal("reserved prefix bypassed original lane", err)
					}
					if !before.Same(imageOf(t, s).Generation()) {
						t.Fatal("ignored reserved-prefix error committed foreign effect")
					}
				})
			}
		})
	}
}

func TestAnchorCommandCompleteCASAndOneUse(t *testing.T) {
	s := openTemp(t)
	anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	first, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	copyFirst := *first
	foreign := openTemp(t)
	if _, err := foreign.AnchorCommand(first, func(context.Context, *CommandTx) error { return nil }); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("foreign Store accepted original update", err)
	}
	proof := anchorTestBootstrap(t, s, dormant)
	receipt, err := s.AnchorCommand(first, func(ctx context.Context, tx *CommandTx) error { return anchorTestStage(ctx, tx, proof.next) })
	if err != nil || receipt.Outcome() != AnchorCommitted {
		t.Fatal("first actual commit failed", err)
	}
	called := false
	if _, err := s.AnchorCommand(&copyFirst, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, retirement.ErrUsed) || called {
		t.Fatal("copy replayed original update", err)
	}
	if _, err := s.AnchorCommand(stale, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, retirement.ErrMismatch) || called {
		t.Fatal("stale original adopted successor cursor", err)
	}
}

func TestAnchorCommandRetainsOriginalWriter(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	before := source.cursor.Load()
	ctx := anchorTestContext(t.Context(), t, s)
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnablePrimary(t.Context(), imageOf(t, s).Generation()); err != nil {
		t.Fatal(err)
	}
	called := false
	receipt, err := s.AnchorCommand(update, func(context.Context, *CommandTx) error { called = true; return nil })
	if !errors.Is(err, replication.ErrFenced) || called || receipt.Outcome() != AnchorRolledBack {
		t.Fatal("old writer was rescued at command entry", err)
	}
	if source.cursor.Load() != before {
		t.Fatal("writer refusal changed accepted cursor")
	}
}

func TestAnchorCommandOriginalFiniteLifetime(t *testing.T) {
	s := openTemp(t)
	anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CaptureAnchorUpdate(WithWriterToken(t.Context(), token), dormant.Version()); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("unbounded acquisition admitted", err)
	}
	ctx, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	called := false
	receipt, err := s.AnchorCommand(update, func(context.Context, *CommandTx) error { called = true; return nil })
	if !errors.Is(err, context.Canceled) || called || receipt.Outcome() != AnchorRolledBack {
		t.Fatal("canceled original reached callback", err)
	}
}

func TestAnchorCommandConfirmedRollbackAndNoop(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	original := source.cursor.Load()
	for _, kind := range []string{"callback-refusal", "final-refusal", "unchanged"} {
		t.Run(kind, func(t *testing.T) {
			ctx := anchorTestContext(t.Context(), t, s)
			update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
			if err != nil {
				t.Fatal(err)
			}
			refusal := errors.New("fixture: original source refused")
			before := imageOf(t, s).Generation()
			receipt, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
				if kind == "unchanged" {
					return anchorTestStage(ctx, tx, dormant)
				}
				if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "foreign/witness", Data: json.RawMessage(`{}`)}); err != nil {
					return err
				}
				if kind == "callback-refusal" {
					return refusal
				}
				return tx.RequireCommitCheck(func(context.Context) error { return refusal })
			})
			if kind == "unchanged" {
				if err != nil || receipt.Outcome() != AnchorUnchanged {
					t.Fatal("no-op lost original admission", err)
				}
			} else if !errors.Is(err, refusal) || receipt.Outcome() != AnchorRolledBack {
				t.Fatal("confirmed rollback misclassified", err)
			}
			if source.cursor.Load() != original || !before.Same(imageOf(t, s).Generation()) {
				t.Fatal("no-op/rollback changed accepted source")
			}
		})
	}
}

func TestAnchorCommandPostimageABAAndSecondRegistration(t *testing.T) {
	for _, tail := range []string{"ABA", "second-registration", "unknown-version"} {
		t.Run(tail, func(t *testing.T) {
			s := openTemp(t)
			anchorTestBind(t, s)
			dormant := anchorTestDormant(t, s)
			ctx := anchorTestContext(t.Context(), t, s)
			update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
			if err != nil {
				t.Fatal(err)
			}
			proof := anchorTestBootstrap(t, s, dormant)
			alternate, err := accepted.ProposeManaged(dormant, proof.state, proof.next.Inspect().UpdatedAt.Add(1), proof.observation.ValidUntil())
			if err != nil {
				t.Fatal(err)
			}
			rows, err := accepted.EncodeRows(alternate)
			if err != nil {
				t.Fatal(err)
			}
			originalRows, err := accepted.EncodeRows(proof.next)
			if err != nil {
				t.Fatal(err)
			}
			var alternateRoot, originalRoot replication.Entry
			for _, row := range rows {
				if row.Key == accepted.RootKey {
					alternateRoot = row
				}
			}
			for _, row := range originalRows {
				if row.Key == accepted.RootKey {
					originalRoot = row
				}
			}
			before := imageOf(t, s).Generation()
			receipt, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
				if err := anchorTestStage(ctx, tx, proof.next); err != nil {
					return err
				}
				if tail == "unknown-version" {
					_ = tx.DeleteSection(ctx, accepted.Section, "trustanchors/v99/root")
					return nil
				}
				if err := tx.PutSection(ctx, accepted.Section, alternateRoot); err != nil {
					return err
				}
				if err := tx.PutSection(ctx, accepted.Section, originalRoot); err != nil {
					return err
				}
				if tail == "second-registration" {
					return tx.RequireSectionPrefixUnchanged(ctx, accepted.Section, accepted.Prefix)
				}
				return nil
			})
			expected := replication.ErrCAS
			if tail == "unknown-version" {
				expected = ErrAnchorUnavailable
			}
			if !errors.Is(err, expected) || receipt.Outcome() != AnchorRolledBack {
				t.Fatal("postimage/ABA fence bypassed", err)
			}
			if !before.Same(imageOf(t, s).Generation()) {
				t.Fatal("refused postimage committed")
			}
		})
	}
}

func TestAnchorCommandRealCommitErrorClosesAdmission(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	for _, statement := range []string{
		`CREATE TEMP TABLE anchor_parent (id INTEGER PRIMARY KEY)`,
		`CREATE TEMP TABLE anchor_child (parent INTEGER REFERENCES anchor_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
	} {
		if _, err := s.w.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	ctx := anchorTestContext(t.Context(), t, s)
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant)
	receipt, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error {
		if err := anchorTestStage(ctx, tx, proof.next); err != nil {
			return err
		}
		_, err := tx.tx.ExecContext(ctx, `INSERT INTO anchor_child(parent) VALUES (1)`)
		return err
	})
	if err == nil || receipt.Outcome() != AnchorUnknown || !source.closed.Load() {
		t.Fatal("failed real Commit reopened admission")
	}
	if _, err := s.CaptureAnchorUpdate(ctx, dormant.Version()); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("unknown commit allowed successor acquisition", err)
	}
}

func TestAnchorCommandUnrelatedWorkDoesNotAcquireRetirement(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	anchorTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	gate := source.cursor.Load().gate
	attempt, err := gate.CaptureWrite(ctx, retirement.Mark{})
	if err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	go func() {
		done <- gate.WithWrite(attempt, func(*retirement.WriteScope) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("original retirement scope did not enter")
	}
	// This finishes while R is still held by the other operation. An ordinary
	// independent-owner command must not be globally serialized behind that R.
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		return tx.PutSection(ctx, "runtime", replication.Entry{Key: "foreign/independent", Data: json.RawMessage(`{}`)})
	})
	if err != nil {
		t.Fatal("unrelated command acquired retirement", err)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("retirement callback did not join")
	}
}

func TestAnchorCommandConfirmedCommitSurvivesPostCommitCancellation(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	original, cancel := context.WithCancel(anchorTestContext(t.Context(), t, s))
	defer cancel()
	update, err := s.CaptureAnchorUpdate(original, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant)
	cell := update.cell
	operation := &anchorOperation{source: cell.source, before: cell.cursor, sql: anchorSQL{outcome: AnchorRolledBack}}
	// Use the ACTUAL private Command body used by AnchorCommand, with controlled
	// cancellation exactly AFTER its durable return and BEFORE gate resolution.
	err = cell.cursor.gate.WithWrite(cell.attempt, func(scope *retirement.WriteScope) error {
		change, err := scope.BeginChange()
		if err != nil {
			return err
		}
		operation.scope, operation.change = scope, change
		err = s.command(cell.original, cell.writer.Term(), func(ctx context.Context, tx *CommandTx) error { return anchorTestStage(ctx, tx, proof.next) }, operation)
		if err != nil {
			return err
		}
		if operation.sql.outcome != AnchorCommitted {
			t.Fatal("command did not actually commit before cancellation")
		}
		cancel()
		return operation.resolve() // No post-effect lifetime reclassification.
	})
	if err != nil || operation.receipt().Outcome() != AnchorCommitted {
		t.Fatal("completed original effect became rollback", err)
	}
	if source.cursor.Load().owner.Version() != proof.next.Version() {
		t.Fatal("postcommit cancellation reopened old owner")
	}
	current := source.cursor.Load()
	later, err := current.gate.CaptureWrite(anchorTestContext(t.Context(), t, s), current.mark)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.gate.WithWrite(later, func(scope *retirement.WriteScope) error { return scope.Check() }); err != nil {
		t.Fatal("gate did not retain confirmed installed mark", err)
	}
}

func TestAnchorCommandUnconfirmedRollbackClosesOriginalGate(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	ctx := anchorTestContext(t.Context(), t, s)
	update, err := s.CaptureAnchorUpdate(ctx, dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("fixture: rollback completion unavailable")
	receipt, err := s.AnchorCommand(update, func(_ context.Context, tx *CommandTx) error {
		// Qualification-only actual SQL termination makes the driver's own rollback
		// return ErrTxDone; that ignored error MUST NOT certify rollback to the gate.
		if err := tx.tx.Rollback(); err != nil {
			return err
		}
		return refusal
	})
	if !errors.Is(err, refusal) || receipt.Outcome() != AnchorUnknown || !source.closed.Load() {
		t.Fatal("unconfirmed rollback reopened original owner", err)
	}
	if _, err := s.BindAnchorLifecycle(ctx); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("Bind reset terminal uncertainty", err)
	}
}

func TestAnchorCommandCloseRefusesRetainedUpdate(t *testing.T) {
	s := openTemp(t)
	anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	update, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := s.AnchorCommand(update, func(context.Context, *CommandTx) error { called = true; return nil }); !errors.Is(err, retirement.ErrClosed) || called {
		t.Fatal("closed Store accepted retained gate operation", err)
	}
}

func TestAnchorCaptureComparesBothOriginalVersionComponents(t *testing.T) {
	s := openTemp(t)
	anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	for _, part := range []string{"envelope", "pure-generation", "pure-digest"} {
		t.Run(part, func(t *testing.T) {
			expected := dormant.Version()
			switch part {
			case "envelope":
				expected.Generation++
			case "pure-generation":
				expected.Pure.Generation++
			case "pure-digest":
				expected.Pure.Digest = expected.Digest
			}
			if _, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), expected); !errors.Is(err, accepted.ErrCAS) {
				t.Fatal("partial owner version admitted", err)
			}
		})
	}
}

func TestAnchorUpdateRequiresAlreadyBoundedOriginalAcquisition(t *testing.T) {
	s := openTemp(t)
	anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	original, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	if _, err := s.CaptureAnchorUpdate(WithWriterToken(original, token), dormant.Version()); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("long original update acquisition admitted", err)
	}
}
