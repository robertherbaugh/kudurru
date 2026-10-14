package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite/sqlitetest"
)

func TestVerifyAuditSnapshotDuringPrune(t *testing.T) {
	for _, operation := range []string{"prune", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := Open(path, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			cutoff := time.Now()
			for i := range 2001 {
				at := cutoff.Add(time.Hour)
				if i < 1500 {
					at = cutoff.Add(-time.Hour)
				}
				if _, err := s.RecordAudit(ctx, AuditEntry{Time: at, Action: "cache.flush"}); err != nil {
					t.Fatal(err)
				}
			}
			original := s.r
			t.Cleanup(func() { _ = original.Close() })
			pages := 0
			s.r = sqlitetest.WithQueryHook(original.Driver(), dsn(path, s.opts.BusyTimeout, false), func(stmt string) {
				if !strings.HasPrefix(stmt, "SELECT id, ts, actor, manager") {
					return
				}
				pages++
				if pages != 2 {
					return
				}
				if operation == "cancel" {
					cancel()
					return
				}
				// The first page has closed, but its read snapshot must remain.
				n, err := s.PruneAudit(context.Background(), cutoff)
				if err != nil || n != 1500 {
					t.Fatalf("concurrent prefix prune: %d, %v", n, err)
				}
				if _, err := s.RecordAudit(context.Background(), AuditEntry{Action: "cache.flush"}); err != nil {
					t.Fatalf("write during verification: %v", err)
				}
			})
			err = s.VerifyAudit(ctx)
			if operation == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled scan: %v", err)
				}
			} else if err != nil || pages < 3 {
				t.Fatalf("retention during a multi-page scan: pages=%d err=%v", pages, err)
			}
			if pages < 2 {
				t.Fatal("verification never reached the scheduled operation")
			}
			// Both successful and canceled scans release their transaction.
			if err := s.VerifyAudit(context.Background()); err != nil {
				t.Fatalf("fresh retained chain: %v", err)
			}
			if operation == "prune" {
				rows, err := s.ReadAudit(context.Background(), AuditQuery{Limit: 1000})
				if err != nil || len(rows) != 502 || rows[0].ID != 1501 {
					t.Fatalf("retained rows: %d, %v", len(rows), err)
				}
			}
		})
	}
}
