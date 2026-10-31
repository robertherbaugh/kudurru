package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestSectionBoundsUseStagedBytesAndPoisonIgnoredFailure(t *testing.T) {
	s := openTemp(t)
	before := imageOf(t, s).Generation()
	const raw = `"😀"` // Six UTF-8 bytes, three SQLite text characters.
	var held *CommandTx
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		held = tx
		if err := tx.CheckSectionBounds(ctx, "security", 0, 0); err != nil {
			return err
		}
		if err := tx.PutSection(ctx, "security", replication.Entry{Key: "unicode", Data: []byte(raw)}); err != nil {
			return err
		}
		if err := tx.CheckSectionBounds(ctx, "security", 1, len(raw)); err != nil {
			return err
		}
		if err := tx.CheckSectionBounds(ctx, "runtime", 0, 0); err != nil {
			return err
		}
		if err := tx.CheckSectionBounds(ctx, "security", 1, len(raw)-1); !errors.Is(err, replication.ErrTooLarge) {
			t.Fatal("preflight counted characters instead of bytes", err)
		}
		return nil // The failed read cannot be ignored to commit staged writes.
	})
	if !errors.Is(err, replication.ErrTooLarge) || !imageOf(t, s).Generation().Same(before) {
		t.Fatal("ignored bounds failure committed the command", err)
	}
	if err := held.CheckSectionBounds(t.Context(), "security", 1, 100); !errors.Is(err, ErrTransactionEnded) {
		t.Fatal("retained transaction admitted section read", err)
	}
}

func TestSectionBoundsRejectRawHistoricalDataBeforePayloadRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rows, bytes int
		seed        string
		args        []any
	}{
		{"bytes", 4096, 4 << 20, `INSERT INTO replication_sections(section,key,data) VALUES ('security','large',CAST(zeroblob(?) AS TEXT))`, []any{4<<20 + 1}},
		{"rows", 1, 10, `INSERT INTO replication_sections(section,key,data) VALUES ('security','a','{}'),('security','b','{}')`, nil},
		{"key", 1, 10, `INSERT INTO replication_sections(section,key,data) VALUES ('security',?,'{}')`, []any{strings.Repeat("k", 513)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			// Simulate raw historical corruption deliberately outside admission.
			if _, err := s.w.ExecContext(t.Context(), tc.seed, tc.args...); err != nil {
				t.Fatal(err)
			}
			err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
				if err := tx.CheckSectionBounds(ctx, "security", tc.rows, tc.bytes); !errors.Is(err, replication.ErrTooLarge) {
					t.Fatal("unbounded historical section accepted", err)
				}
				return nil
			})
			if !errors.Is(err, replication.ErrTooLarge) {
				t.Fatal("no-op bypassed bounds poison", err)
			}
		})
	}
}

func TestSectionBoundsValidateBudgetsAndCancellation(t *testing.T) {
	s := openTemp(t)
	for _, tc := range []struct {
		section     string
		rows, bytes int
	}{
		{"unknown", 1, 100}, {"security", -1, 100}, {"security", 1, -1},
		{"security", 100001, 100}, {"security", 1, 256<<20 + 1},
	} {
		err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			_ = tx.CheckSectionBounds(ctx, tc.section, tc.rows, tc.bytes)
			return nil
		})
		if !errors.Is(err, replication.ErrInvalid) {
			t.Fatal("bad section budget admitted", tc, err)
		}
	}
	err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_ = tx.CheckSectionBounds(canceled, "security", 1, 100)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("canceled preflight admitted", err)
	}
}
