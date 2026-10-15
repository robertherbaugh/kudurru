package spool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite/sqlitetest"
)

func TestVerifySnapshotDuringRetention(t *testing.T) {
	for _, operation := range []string{"prune", "cap", "cancel"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s := open(t, Options{Max: 2001})
			cutoff := time.Now()
			for i := range 2001 {
				at := cutoff.Add(time.Hour)
				if i < 1500 {
					at = cutoff.Add(-time.Hour)
				}
				if err := s.Record(ctx, audit.Event{Time: at, Action: "cache.flush"}); err != nil {
					t.Fatal(err)
				}
			}
			original := s.db
			t.Cleanup(func() { _ = original.Close() })
			pages := 0
			s.db = sqlitetest.WithQueryHook(original.Driver(), s.opts.Path, func(stmt string) {
				if !strings.HasPrefix(stmt, "SELECT id, doc FROM event WHERE id > ?") {
					return
				}
				pages++
				if pages != 2 {
					return
				}
				switch operation {
				case "cancel":
					cancel()
				case "prune":
					n, err := s.Prune(context.Background(), cutoff)
					if err != nil || n != 1500 {
						t.Fatalf("concurrent prefix prune: %d, %v", n, err)
					}
				case "cap":
					// Writes advance the capacity cap past the first scan page.
					for range 1500 {
						if err := s.Record(context.Background(), audit.Event{Action: "cache.flush"}); err != nil {
							t.Fatalf("capped write during verification: %v", err)
						}
					}
				}
			})
			s.db.SetMaxOpenConns(4)
			err := s.Verify(ctx)
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
			if err := s.Verify(context.Background()); err != nil {
				t.Fatalf("fresh retained chain: %v", err)
			}
			if operation != "cancel" {
				rows, err := s.ReadAfter(context.Background(), 0, 10000)
				want := 501
				if operation == "cap" {
					want = 2001
				}
				if err != nil || len(rows) != want || rows[0].ID != 1501 {
					t.Fatalf("retained rows: %d, %v", len(rows), err)
				}
			}
		})
	}
}
