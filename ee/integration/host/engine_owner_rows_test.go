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
// owner-wide reads that cleanup (D367) depends on must count them, must not
// count or return another incarnation's rows, and must refuse a corrupt row
// instead of concluding that less is owned.
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
	// Three ledger rows are stored; two are this incarnation's, one on each
	// destination, and the one on the destination the spec dropped is stranded.
	if stored, err := st.List(ctx, kinds.KindIntegrationLedger, store.ListOptions{}); err != nil || len(stored.Items) != 3 {
		t.Fatalf("stored ledger rows: %d %v", len(stored.Items), err)
	}
	total, stranded, err := h.ledgerCounts(ctx, owner, []kinds.Destination{second})
	if err != nil || total != 2 || stranded != 1 {
		t.Fatalf("owner rows across destinations: total=%d stranded=%d %v", total, stranded, err)
	}
	if removed, err := h.ledgers(ctx, owner, first); err != nil || len(removed) != 1 || removed[0].Spec.Record.Owner != owner || removed[0].ResourceVersion == "" {
		t.Fatalf("the stranded row is not the removed destination's: %d %v", len(removed), err)
	}
	if _, none, err := h.ledgerCounts(ctx, owner, []kinds.Destination{first, second}); err != nil || none != 0 {
		t.Fatalf("stranded rows with every destination published: %d %v", none, err)
	}
	if all, every, err := h.ledgerCounts(ctx, owner, nil); err != nil || all != 2 || every != 2 {
		t.Fatalf("stranded rows do not follow the published destinations: %d %d %v", all, every, err)
	}
	operations, err := h.operationBatch(ctx, owner)
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
	if rows, _, err := h.ledgerCounts(ctx, previous, nil); err != nil || rows != 1 {
		t.Fatalf("previous incarnation rows: %d %v", rows, err)
	}
	// Returned rows are copies.
	configured[0].Spec.Sequence = 99
	if again, _ := h.ledgers(ctx, owner, second); again[0].Spec.Sequence == 99 {
		t.Fatal("ledger rows alias stored state")
	}
	operations[0].Spec.Action = "aliased"
	if again, _ := h.operationBatch(ctx, owner); again[0].Spec.Action == "aliased" || again[1].Spec.Action == "aliased" {
		t.Fatal("owner operations alias stored state")
	}

	// A row of this owner that does not validate is an error, never an
	// omission: cleanup must not finish over state it could not read.
	corrupt := kinds.RecordIdentity{Owner: owner, Destination: first, Key: kinds.RecordKey{Name: "b.example.com.", Type: "A"}}
	if _, err := st.Apply(ctx, &kinds.IntegrationLedger{ObjectMeta: metav1.ObjectMeta{Name: kinds.LedgerName(corrupt)}, Spec: kinds.IntegrationLedgerSpec{FormatVersion: kinds.FormatVersion, Record: corrupt}}, store.ApplyOptions{Manager: "test", CreateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledgerCounts(ctx, owner, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("corrupt owner row: %v", err)
	}
	if _, err := h.ledgers(ctx, owner, second); err != nil {
		t.Fatalf("a corrupt row of another destination changed ordinary planning: %v", err)
	}
}
