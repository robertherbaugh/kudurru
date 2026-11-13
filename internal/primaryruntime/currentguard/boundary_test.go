package currentguard

import (
	"context"
	"errors"
	"testing"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// A boundary-only transaction (D404) accepts the same zone whose receipt is
// behind a status write; every other check still needs the exact receipt,
// and a boundary check still needs the zone's identity.
func TestBackgroundBoundaryAcceptsSameZoneBehindItsReceipt(t *testing.T) {
	f := newSQLiteFixture(t)
	l := f.active("primary", "example.")
	var source public.DNSZone
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	source.Status.Serial++
	if err := f.s.UpdateStatus(f.ctx(), &source); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Get(f.ctx(), store.Key{Kind: "DNSZone", Name: "primary"}, &source); err != nil {
		t.Fatal(err)
	}
	if source.ResourceVersion == l.Spec.Source.ResourceVersion {
		t.Fatal("fixture did not put the receipt behind its DNSZone")
	}
	other := source.DeepCopy()
	other.UID = "other-uid"
	check := func(c sqliteadapter.BackgroundCheck) error {
		return f.g.Background().WithCurrent(f.ctx(), f.fence, func(check sqliteadapter.BackgroundPolicy) error {
			return f.s.Command(f.ctx(), f.fence.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return check(ctx, tx, c)
			})
		})
	}
	if err := check(sqliteadapter.BackgroundCheck{Lifecycle: l, Source: &source, Reason: "lifecycle"}); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("an exact check accepted a receipt behind its DNSZone", err)
	}
	if err := check(sqliteadapter.BackgroundCheck{Lifecycle: l, Source: &source, Reason: "lifecycle", Boundary: true}); err != nil {
		t.Fatal("a boundary check refused the same zone", err)
	}
	if err := check(sqliteadapter.BackgroundCheck{Lifecycle: l, Source: other, Reason: "lifecycle", Boundary: true}); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("a boundary check accepted another zone", err)
	}
	if err := check(sqliteadapter.BackgroundCheck{Lifecycle: l, Source: &source, Reason: "source-reconcile", Boundary: true}); !errors.Is(err, pr.ErrUnavailable) {
		t.Fatal("a boundary flag relaxed a non-boundary command", err)
	}
}
