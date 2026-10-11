package sqlitelog

import (
	"context"
	"testing"
	"time"
)

func TestPurge(t *testing.T) {
	w, _ := newWriter(t, t0)
	w.deleteBatch = 3 // several batches
	for i := range 10 {
		mustWrite(t, w, event(t0.Add(time.Duration(i)*time.Minute), "10.0.0.1", "a.example"))
	}
	ctx := context.Background()
	n, err := w.Purge(ctx, t0.Add(4*time.Minute))
	if err != nil || n != 4 {
		t.Fatalf("purge before: %d %v", n, err)
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != 6 {
		t.Fatalf("rows left %d", got)
	}
	if w.rows.Load() != 6 {
		t.Fatalf("row counter %d", w.rows.Load())
	}
	n, err = w.Purge(ctx, time.Time{})
	if err != nil || n != 6 {
		t.Fatalf("purge all: %d %v", n, err)
	}
	if got := queryInt(t, w.w, `SELECT count(*) FROM query`); got != 0 {
		t.Fatalf("rows left %d", got)
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := queryInt(t, w.w, `SELECT sum(total) FROM rollup_1m`); got != 10 {
		t.Fatalf("rollups must survive a purge: %d", got)
	}
}
