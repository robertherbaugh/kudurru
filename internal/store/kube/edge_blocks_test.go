package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// TestEdgeBlocksActualCASAndRefusal runs against envtest: a create of the
// shared runtime ConfigMap, a second driver observing it, coexistence with
// the blocking annotation and unrelated data, a stale update refused by CAS
// without retry, and malformed state refused instead of repaired.
func TestEdgeBlocksActualCASAndRefusal(t *testing.T) {
	requireEnv(t)
	ns := blockingNamespace(t)
	a := newTestStore(t, Options{Namespace: ns, Identity: "edge-a"})
	b := newTestStore(t, Options{Namespace: ns, Identity: "edge-b"})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	absent, err := a.ReadEdgeBlocks(ctx)
	if err != nil || absent.UID != "" || len(absent.State.Blocks) != 0 {
		t.Fatal("missing runtime is not the empty set", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	add := func(prefix, id string) func(edgeblock.State) (edgeblock.State, error) {
		return func(s edgeblock.State) (edgeblock.State, error) {
			next, _, err := edgeblock.Add(s, edgeblock.Request{Prefix: prefix, Duration: time.Hour}, id, now)
			return next, err
		}
	}
	admissions := 0
	admit := func(context.Context) error { admissions++; return nil }
	receipt, err := a.ChangeEdgeBlocks(ctx, add("192.0.2.0/24", "00000000000000a1"), admit)
	if err != nil || !receipt.Committed || admissions != 2 || receipt.After.UID == "" || receipt.After.State.Revision != 1 {
		t.Fatal("create failed", err, receipt)
	}
	// The blocking toggle shares the ConfigMap; both survive each other.
	if _, err := b.ChangeBlockingRuntime(ctx, now.Add(time.Minute), admit); err != nil {
		t.Fatal(err)
	}
	receipt, err = b.ChangeEdgeBlocks(ctx, add("198.51.100.0/24", "00000000000000b2"), admit)
	if err != nil || receipt.After.State.Revision != 2 || len(receipt.After.State.Blocks) != 2 {
		t.Fatal("second change failed", err, receipt)
	}
	blocking, err := a.ReadBlockingRuntime(ctx)
	if err != nil || !blocking.Until.Equal(now.Add(time.Minute)) {
		t.Fatal("edge block change lost the blocking deadline", err)
	}
	seen, err := a.ReadEdgeBlocks(ctx)
	if err != nil || seen.State.Revision != 2 || seen.UID != receipt.After.UID {
		t.Fatal("second driver does not observe the set", err, seen)
	}
	key := client.ObjectKey{Namespace: ns, Name: BlockingRuntimeName}
	t.Run("stale", func(t *testing.T) {
		calls := 0
		receipt, err := a.ChangeEdgeBlocks(ctx, add("203.0.113.0/24", "00000000000000c3"), func(context.Context) error {
			calls++
			if calls == 2 {
				var cm corev1.ConfigMap
				if err := shared.admin.Get(ctx, key, &cm); err != nil {
					t.Fatal(err)
				}
				cm.Data["unrelated"] = "competing"
				if err := shared.admin.Update(ctx, &cm); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		if !apierrors.IsConflict(err) || receipt.Committed || !receipt.Dispatched || calls != 2 {
			t.Fatal("stale change bypassed CAS or retried", err, receipt)
		}
	})
	t.Run("model-refusal", func(t *testing.T) {
		receipt, err := a.ChangeEdgeBlocks(ctx, add("192.0.2.0/24", "00000000000000d4"), admit)
		if !errors.Is(err, edgeblock.ErrExists) || receipt.Dispatched {
			t.Fatal("duplicate prefix dispatched", err, receipt)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		var cm corev1.ConfigMap
		if err := shared.admin.Get(ctx, key, &cm); err != nil {
			t.Fatal(err)
		}
		cm.Data[edgeblock.ConfigMapKey] = `{"formatVersion":9}`
		if err := shared.admin.Update(ctx, &cm); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ReadEdgeBlocks(ctx); !errors.Is(err, edgeblock.ErrState) {
			t.Fatal("malformed state was read", err)
		}
		receipt, err := a.ChangeEdgeBlocks(ctx, add("203.0.113.0/24", "00000000000000c3"), admit)
		if !errors.Is(err, edgeblock.ErrState) || receipt.Dispatched {
			t.Fatal("command repaired malformed state", err, receipt)
		}
	})
}
