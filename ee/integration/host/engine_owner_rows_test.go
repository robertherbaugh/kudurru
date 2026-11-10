package host

import (
	"context"
	"errors"
	"testing"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Ordinary planning reads the ledger one configured destination at a time, so
// the rows of a destination that left the spec are invisible to it. The
// owner-wide reads that cleanup (D367) depends on must return them, must not
// return another incarnation's rows, and must refuse a corrupt row instead of
// concluding that less is owned.
func TestOwnerLedgersAndOperationsSpanDestinationsRemovedFromSpec(t *testing.T) {
	h, j, p, st, _ := engineFixture(t)
	ctx := context.Background()
	f, g, _ := h.captured(j)
	owner := f.instance.owner
	first := p.pubs[0].Destination
	if res, err := h.sync(ctx, j, f, g, ""); err != nil || res.Applied != 1 {
		t.Fatal("first destination was not published", err)
	}
	// The spec now names another zone only.
	second := first
	second.Zone = "example.org"
	p.pubs = []Publication{{Destination: second, Domains: []string{"example.com"}}}
	p.remote[second] = RemoteSnapshot{Complete: true}
	if res, err := h.sync(ctx, j, f, g, ""); err != nil || res.Applied != 1 {
		t.Fatal("second destination was not published", err)
	}
	// A previous incarnation of the same name left its own row behind.
	previous := owner
	previous.UID = "uid-previous"
	foreign := kinds.RecordIdentity{Owner: previous, Destination: first, Key: kinds.RecordKey{Name: "a.example.com.", Type: "A"}}
	if _, err := st.Apply(ctx, &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(foreign)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: foreign, Sequence: 1}}, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}

	configured, err := h.ledgers(ctx, owner, second)
	if err != nil || len(configured) != 1 || configured[0].Spec.Record.Destination != second {
		t.Fatalf("configured destination rows: %d %v", len(configured), err)
	}
	all, err := h.ownerLedgers(ctx, owner)
	if err != nil || len(all) != 2 {
		t.Fatalf("owner rows across destinations: %d %v", len(all), err)
	}
	for _, l := range all {
		if l.Spec.Record.Owner != owner || l.ResourceVersion == "" {
			t.Fatalf("row of another incarnation or without a version: %+v", l.Spec.Record)
		}
	}
	stranded := strandedLedgers(all, []kinds.Destination{second})
	if len(stranded) != 1 || stranded[0].Spec.Record.Destination != first {
		t.Fatalf("rows of the removed destination were not reported: %+v", stranded)
	}
	if len(strandedLedgers(all, []kinds.Destination{first, second})) != 0 || len(strandedLedgers(all, nil)) != 2 {
		t.Fatal("stranded rows do not follow the published destinations")
	}
	operations, err := h.ownerOperations(ctx, owner)
	if err != nil || len(operations) != 2 {
		t.Fatalf("owner operations: %d %v", len(operations), err)
	}
	destinations := map[kinds.Destination]bool{}
	for _, o := range operations {
		if o.Spec.Record.Owner != owner || o.Spec.Receipt == nil || !o.Spec.Receipt.Complete || o.ResourceVersion == "" {
			t.Fatalf("operation: %+v", o.Spec.Record)
		}
		destinations[o.Spec.Record.Destination] = true
	}
	if !destinations[first] || !destinations[second] {
		t.Fatalf("operations do not span both destinations: %v", destinations)
	}
	if rows, err := h.ownerLedgers(ctx, previous); err != nil || len(rows) != 1 {
		t.Fatalf("previous incarnation rows: %d %v", len(rows), err)
	}
	// Returned rows are copies.
	all[0].Spec.Sequence = 99
	if again, _ := h.ownerLedgers(ctx, owner); again[0].Spec.Sequence == 99 {
		t.Fatal("owner rows alias stored state")
	}

	// A row of this owner that does not validate is an error, never an
	// omission: cleanup must not finish over state it could not read.
	corrupt := kinds.RecordIdentity{Owner: owner, Destination: first, Key: kinds.RecordKey{Name: "b.example.com.", Type: "A"}}
	if _, err := st.Apply(ctx, &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(corrupt)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: corrupt}}, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ownerLedgers(ctx, owner); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt owner row: %v", err)
	}
	if _, err := h.ledgers(ctx, owner, second); err != nil {
		t.Fatalf("a corrupt row of another destination changed ordinary planning: %v", err)
	}
}
