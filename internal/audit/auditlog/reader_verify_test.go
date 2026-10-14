package auditlog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	auditv1 "github.com/robertherbaugh/kudurru/api/audit/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReaderVerifyRotationAndPending(t *testing.T) {
	ctx := context.Background()
	previous, _ := SealKey([]byte("previous-encryption-key"))
	current, _ := SealKey([]byte("current-encryption-key"))
	c := fake.NewClientBuilder().WithScheme(testScheme()).Build()
	sp := openSpool(t, "node-a")
	target := &Target{Client: c, Namespace: "audit", SealKey: previous}
	// Create three linked batches, with the first two written before rotation.
	for i := range 3 {
		if i == 2 {
			target.SealKey = current
		}
		if err := sp.Record(ctx, event(time.Now().Add(time.Duration(i)*time.Second), "alice", "DNSZone", fmt.Sprintf("z%d", i))); err != nil {
			t.Fatal(err)
		}
		ship(t, target, sp)
	}
	var batches auditv1.AuditLogList
	if err := c.List(ctx, &batches); err != nil || len(batches.Items) != 3 {
		t.Fatalf("batches: %d, %v", len(batches.Items), err)
	}
	pendingCalls := 0
	reader := &Reader{Client: c, Namespace: "audit", SealKeys: [][]byte{current, previous}, VerifyPending: func(ctx context.Context) error {
		pendingCalls++
		return sp.Verify(ctx)
	}}
	if err := reader.Verify(ctx); err != nil || pendingCalls != 1 {
		t.Fatalf("rotation with good spool: %v, pending calls %d", err, pendingCalls)
	}
	reader.SealKeys = [][]byte{current}
	if err := reader.Verify(ctx); !errors.Is(err, audit.ErrTampered) {
		t.Fatal("old batches verified without their seal key")
	}
	reader.SealKeys = [][]byte{current, previous}
	want := errors.New("pending spool unavailable")
	reader.VerifyPending = func(context.Context) error { return want }
	if err := reader.Verify(ctx); !errors.Is(err, want) {
		t.Fatalf("pending error: %v", err)
	}
	reader.VerifyPending = sp.Verify
	for i := range batches.Items {
		if batches.Items[i].Spec.FirstID != 2 {
			continue
		}
		middle := batches.Items[i].DeepCopy()
		middle.Spec.Events[0].Reason = "edited"
		if err := c.Update(ctx, middle); err != nil {
			t.Fatal(err)
		}
		if err := reader.Verify(ctx); !errors.Is(err, audit.ErrTampered) {
			t.Fatal("edited batch verified")
		}
		if err := c.Delete(ctx, middle); err != nil {
			t.Fatal(err)
		}
		if err := reader.Verify(ctx); !errors.Is(err, audit.ErrTampered) {
			t.Fatal("missing middle batch verified")
		}
		return
	}
	t.Fatal("middle batch not found")
}
