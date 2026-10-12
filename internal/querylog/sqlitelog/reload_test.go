package sqlitelog

import (
	"context"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
)

// TestSetPrivacyAndRetention: querylog.privacy and querylog.retention.*
// apply to batches written after the change, without reopening.
func TestSetPrivacyAndRetention(t *testing.T) {
	w, _ := newWriter(t, t0)
	mustWrite(t, w, event(t0, "10.0.0.1", "a.example"))
	if n := queryInt(t, w.w, `SELECT count(*) FROM query`); n != 1 {
		t.Fatalf("full: %d fact rows", n)
	}
	w.SetPrivacy(querylog.PrivacyNothing)
	mustWrite(t, w, event(t0.Add(time.Second), "10.0.0.2", "b.example"))
	if n := queryInt(t, w.w, `SELECT count(*) FROM query`); n != 1 {
		t.Fatalf("nothing must store no fact row: %d", n)
	}
	w.SetRetention(2*time.Hour, 1<<30)
	st, err := w.Reader().Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Privacy != querylog.PrivacyNothing.String() || st.Retention.RawSeconds != 7200 || st.Retention.MaxDBBytes != 1<<30 {
		t.Fatalf("status after the change: %+v", st)
	}
	if w.Reader().factTier().keep != 2*time.Hour {
		t.Fatal("the reader tier must follow the raw retention")
	}
}
