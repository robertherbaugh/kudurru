package runtimeowners

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/security"
	inventorypersistence "github.com/robertherbaugh/kudurru/internal/inventory/persistence"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	anchorpersistence "github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
)

var anchorOwnerValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func anchorOwnerImage(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	image, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func initializeAnchorOwners(t *testing.T, s *sqlite.Store) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return Initialize(ctx, tx, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
}

func requireAnchorOwnerUnchanged(t *testing.T, s *sqlite.Store, before *replication.Image) {
	t.Helper()
	after := anchorOwnerImage(t, s)
	if !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
		t.Fatal("refused owner command changed original complete image")
	}
}

func TestAnchorOwnerStartupExistingInventoryThenRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	s := openOwnerStore(t, path)
	// This is the actual upgrade ordering: inventory/security may already exist
	// while the newly physical-migrated anchor owner is still only a sentinel.
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := security.InitializeOwners(ctx, tx); err != nil {
			return err
		}
		_, err := inventorypersistence.Initialize(ctx, tx, tx.Authority().ClusterID, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := anchorOwnerImage(t, s)
	if !accepted.IsAwaitingOwner(before.Payload.Runtime) {
		t.Fatal("fixture lost physical sentinel")
	}
	if _, token, err := s.ReadServingCandidate(t.Context()); !errors.Is(err, accepted.ErrInvalid) || token.Generation().Digest != "" {
		t.Fatal("uninitialized anchor owner received serving token", err)
	}
	if _, err := s.EnablePrimary(t.Context(), before.Generation()); !errors.Is(err, accepted.ErrInvalid) {
		t.Fatal("sentinel enabled primary", err)
	}
	initializeAnchorOwners(t, s)
	first := anchorOwnerImage(t, s)
	owner, err := accepted.DecodeRows(first.Payload.Runtime, first.Term.ClusterID)
	if err != nil || owner.Inspect().Phase != accepted.Dormant || owner.Inspect().HistoryID == "" || owner.Version().Generation != 1 {
		t.Fatal("startup did not bind never-managed owner", err)
	}
	if first.Revision != before.Revision+1 || first.ObjectResourceVersion != before.ObjectResourceVersion || !reflect.DeepEqual(before.Payload.Security, first.Payload.Security) {
		t.Fatal("anchor startup changed unrelated authority or counters")
	}
	oldInventory, err := inventorypersistence.Decode(before.Payload.Runtime, before.Term.ClusterID, before.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	newInventory, err := inventorypersistence.Decode(first.Payload.Runtime, first.Term.ClusterID, first.CreatedAt)
	if err != nil || oldInventory.Version != newInventory.Version {
		t.Fatal("anchor startup rewrote accepted inventory", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openOwnerStore(t, path)
	initializeAnchorOwners(t, s)
	requireAnchorOwnerUnchanged(t, s, first)
	restored, err := accepted.DecodeRows(anchorOwnerImage(t, s).Payload.Runtime, first.Term.ClusterID)
	if err != nil || restored.Version() != owner.Version() || !reflect.DeepEqual(restored.Inspect(), owner.Inspect()) {
		t.Fatal("restart reset anchor history", err)
	}
}

func TestAnchorOwnerMissingHistoryRefusesEvenWithExistingInventory(t *testing.T) {
	for _, priorInventory := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-inventory", true: "existing-inventory"}[priorInventory], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sqlite.FileName)
			s := openOwnerStore(t, path)
			if priorInventory {
				initializeAnchorOwners(t, s)
			}
			if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return tx.DeleteSection(ctx, accepted.Section, accepted.RootKey)
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openOwnerStore(t, path) // Current schema must never re-create sentinel.
			before := anchorOwnerImage(t, s)
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, time.Now().UTC()) })
			if !errors.Is(err, accepted.ErrMissing) {
				t.Fatal("startup reconstructed missing current-schema history", err)
			}
			requireAnchorOwnerUnchanged(t, s, before)
			if _, _, err := s.ReadServingCandidate(t.Context()); err == nil {
				t.Fatal("incomplete anchor owner served")
			}
		})
	}
}

func TestAnchorOwnerInitializationRollbackAndPostimageFence(t *testing.T) {
	for _, alreadyInitialized := range []bool{false, true} {
		for _, stage := range []string{"final-refusal", "prefix-aba", "cancel"} {
			t.Run(map[bool]string{false: "sentinel", true: "unchanged"}[alreadyInitialized]+"/"+stage, func(t *testing.T) {
				s := openOwnerStore(t, filepath.Join(t.TempDir(), sqlite.FileName))
				if alreadyInitialized {
					initializeAnchorOwners(t, s)
				}
				before := anchorOwnerImage(t, s)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				refused := errors.New("fixture: original startup refused")
				err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if err := Initialize(ctx, tx, time.Now().UTC()); err != nil {
						return err
					}
					switch stage {
					case "final-refusal":
						return tx.RequireCommitCheck(func(context.Context) error { return refused })
					case "prefix-aba":
						data, err := tx.GetSection(ctx, accepted.Section, accepted.RootKey)
						if err != nil {
							return err
						}
						if err := tx.DeleteSection(ctx, accepted.Section, accepted.RootKey); err != nil {
							return err
						}
						return tx.PutSection(ctx, accepted.Section, replication.Entry{Key: accepted.RootKey, Data: data})
					case "cancel":
						cancel()
					}
					return nil
				})
				want := refused
				if stage == "prefix-aba" {
					want = replication.ErrCAS
				}
				if stage == "cancel" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatal("startup crossed original final/postimage refusal", err)
				}
				requireAnchorOwnerUnchanged(t, s, before)
			})
		}
	}
}

func TestAnchorOwnerRejectsMissingCallbackWriterBeforeOwnerWrites(t *testing.T) {
	s := openOwnerStore(t, filepath.Join(t.TempDir(), sqlite.FileName))
	before := anchorOwnerImage(t, s)
	err := s.Command(t.Context(), s.Authority().Term, func(_ context.Context, tx *sqlite.CommandTx) error {
		// Outer Command remains admitted. Composition must not use its fallback
		// to adopt a caller context that lost the ORIGINAL callback token.
		return Initialize(t.Context(), tx, time.Now().UTC())
	})
	if !errors.Is(err, anchorpersistence.ErrOriginal) {
		t.Fatal("composition sampled a replacement writer", err)
	}
	requireAnchorOwnerUnchanged(t, s, before)
}

func TestAnchorOwnerCompleteValidationRejectsMissingSentinelAndOrphan(t *testing.T) {
	s := openOwnerStore(t, filepath.Join(t.TempDir(), sqlite.FileName))
	initializeAnchorOwners(t, s)
	base := anchorOwnerImage(t, s)
	for _, stage := range []string{"missing", "sentinel", "unknown", "orphan"} {
		t.Run(stage, func(t *testing.T) {
			image := *base
			image.Payload.Runtime = nil
			for _, row := range base.Payload.Runtime {
				if row.Key != accepted.RootKey {
					image.Payload.Runtime = append(image.Payload.Runtime, row)
				}
			}
			switch stage {
			case "sentinel":
				image.Payload.Runtime = append(image.Payload.Runtime, replication.Entry{Key: accepted.RootKey, Data: json.RawMessage(accepted.AwaitingOwnerJSON)})
			case "unknown":
				image.Payload.Runtime = append(image.Payload.Runtime, replication.Entry{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)})
			case "orphan":
				image.Payload.Runtime = append([]replication.Entry(nil), base.Payload.Runtime...)
				image.Payload.Runtime = append(image.Payload.Runtime, replication.Entry{Key: accepted.ChunkPrefix + "orphan", Data: json.RawMessage(`{}`)})
			}
			if err := security.ValidateImage(&image); err != nil {
				t.Fatal("fixture removed unrelated security owner", err)
			}
			if err := inventorypersistence.ValidateImage(&image); err != nil {
				t.Fatal("fixture removed unrelated inventory owner", err)
			}
			want := accepted.ErrInvalid
			if stage == "missing" {
				want = accepted.ErrMissing
			}
			if err := ValidateImage(&image); !errors.Is(err, want) {
				t.Fatal("complete validation accepted incomplete anchor history", err)
			}
			requireAnchorOwnerUnchanged(t, s, base)
		})
	}
	if err := ValidateEntry(accepted.Section, replication.Entry{Key: accepted.RootKey, Data: json.RawMessage(accepted.AwaitingOwnerJSON)}); !errors.Is(err, accepted.ErrInvalid) {
		t.Fatal("ordinary entry accepted physical sentinel", err)
	}
}

func TestAnchorOwnerMalformedStartupCannotRepairHistory(t *testing.T) {
	for _, stage := range []string{"malformed-root", "orphan", "unknown-version"} {
		t.Run(stage, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sqlite.FileName)
			s := openOwnerStore(t, path)
			initializeAnchorOwners(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			switch stage {
			case "malformed-root":
				_, err = db.ExecContext(t.Context(), `UPDATE replication_sections SET data=? WHERE section=? AND key=?`, `{"formatVersion":1,"phase":"awaiting-owner","alias":true}`, accepted.Section, accepted.RootKey)
			case "orphan":
				_, err = db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, accepted.ChunkPrefix+"orphan", `{}`)
			case "unknown-version":
				_, err = db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, "trustanchors/v99/root", `{}`)
			}
			closeErr := db.Close()
			if err != nil || closeErr != nil {
				t.Fatal("prepare controlled owner corruption", err, closeErr)
			}
			s = openOwnerStore(t, path)
			before := anchorOwnerImage(t, s)
			err = s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return Initialize(ctx, tx, time.Now().UTC()) })
			if !errors.Is(err, accepted.ErrInvalid) {
				t.Fatal("startup repaired malformed current-schema history", err)
			}
			requireAnchorOwnerUnchanged(t, s, before)
		})
	}
}
