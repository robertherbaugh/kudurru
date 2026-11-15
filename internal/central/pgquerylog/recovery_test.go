package pgquerylog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/central/centraltest"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	testclock "k8s.io/utils/clock/testing"
)

func TestRecoveryReceiptsScopeStaleReplayAndFinalCheck(t *testing.T) {
	db := centraltest.New(t)
	fc := testclock.NewFakeClock(t0.Add(-30 * time.Minute))
	ctx := t.Context()
	w := writer(t, "receipt-node", fc)
	write(t, w, ev("receipt-node", fc.Now(), "192.0.2.1", "receipt.example", pipeline.StatusForwarded))
	s := shipper(t, db, w, "receipt-node", fc)
	shipAll(t, s)
	hour := fc.Now().Unix() / 3600 * 3600
	fc.Step(3 * time.Hour)
	r := Recovery{DB: db, Now: fc.Now}
	e, err := r.Evidence(ctx, s.source, "receipt-node", hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(ctx, *e); err != nil {
		t.Fatal(err)
	}
	changedExpiry := *e
	changedExpiry.ExpiresAt = changedExpiry.ExpiresAt.Add(-time.Minute)
	if err := r.Verify(ctx, changedExpiry); !errors.Is(err, sqlitelog.ErrRollupStale) {
		t.Fatal("changed expiry accepted", err)
	}
	altered := *e
	altered.Node = "other"
	if err := r.Verify(ctx, altered); err == nil {
		t.Fatal("cross-node receipt accepted")
	}
	altered = *e
	altered.Revision++
	if err := r.Verify(ctx, altered); !errors.Is(err, sqlitelog.ErrRollupStale) {
		t.Fatal("changed revision accepted", err)
	}
	calls := 0
	check := func(context.Context) error { calls++; return nil }
	result, err := r.Repair(ctx, *e, check)
	if err != nil || !result.Applied || calls != 2 {
		t.Fatalf("apply=%+v err=%v checks=%d", result, err, calls)
	}
	result, err = r.Repair(ctx, *e, check)
	if err != nil || !result.AlreadyApplied || scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 {
		t.Fatalf("duplicate=%+v: %v", result, err)
	}
	// A duplicate still requires current authority.
	denied := errors.New("authority revoked")
	if _, err := r.Repair(ctx, *e, func(context.Context) error { return denied }); !errors.Is(err, denied) {
		t.Fatal("duplicate bypassed current authority")
	}
	// New retained input invalidates the old receipt and the synthetic bucket.
	write(t, w, ev("receipt-node", time.Unix(hour+1, 0), "192.0.2.2", "late.example", pipeline.StatusCached))
	if _, err := s.shipFacts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Repair(ctx, *e, check); !errors.Is(err, sqlitelog.ErrRollupStale) {
		t.Fatal("late input did not stale receipt", err)
	}
	if err := s.shipRollups(ctx, prunePool(t, db)); err != nil {
		t.Fatal(err)
	}
	fresh, err := r.Evidence(ctx, s.source, "receipt-node", hour)
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := r.Repair(ctx, *fresh, func(context.Context) error {
		calls++
		if calls == 2 {
			return denied
		}
		return nil
	}); !errors.Is(err, denied) {
		t.Fatal("final authority not checked", err)
	}
	if scalar(t, db, `SELECT sum(total) FROM rollup_1h`) != 1 || scalar(t, db, `SELECT count(*) FROM rollup_repair_receipt`) != 1 {
		t.Fatal("failed final check committed summary/receipt")
	}
	fc.Step(6 * time.Minute)
	if err := r.Verify(ctx, *fresh); !errors.Is(err, sqlitelog.ErrRollupStale) {
		t.Fatal("expired receipt accepted", err)
	}
}
