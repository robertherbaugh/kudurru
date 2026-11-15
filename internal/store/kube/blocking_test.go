package kube

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func blockingNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "blocking-"}}
	if err := shared.admin.Create(t.Context(), ns); err != nil {
		t.Fatal(err)
	}
	return ns.Name
}

func TestBlockingRuntimeActualCASAndRefusal(t *testing.T) {
	requireEnv(t)
	ns := blockingNamespace(t)
	a := newTestStore(t, Options{Namespace: ns, Identity: "blocking-a"})
	b := newTestStore(t, Options{Namespace: ns, Identity: "blocking-b"})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	absent, err := a.ReadBlockingRuntime(ctx)
	if err != nil || absent.UID != "" || !absent.Until.IsZero() {
		t.Fatal("missing runtime did not observe enabled", err)
	}
	var cm corev1.ConfigMap
	key := client.ObjectKey{Namespace: ns, Name: BlockingRuntimeName}
	if err := shared.admin.Get(ctx, key, &cm); !apierrors.IsNotFound(err) {
		t.Fatal("read initialized missing runtime", err)
	}
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	admissions := 0
	admit := func(context.Context) error { admissions++; return nil }
	receipt, err := a.ChangeBlockingRuntime(ctx, until, admit)
	if err != nil || !receipt.Dispatched || !receipt.Committed || admissions != 2 || receipt.After.UID == "" || !receipt.After.Until.Equal(until) {
		t.Fatal("actual runtime create failed", err, receipt)
	}
	seen, err := b.ReadBlockingRuntime(ctx)
	if err != nil || seen != receipt.After {
		t.Fatal("second driver lost original absolute expiry", err, seen)
	}
	if err := shared.admin.Get(ctx, key, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data = map[string]string{"unrelated": "retained"}
	cm.Annotations["example.test/owner"] = "retained"
	if err := shared.admin.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	receipt, err = b.ChangeBlockingRuntime(ctx, time.Time{}, admit)
	if err != nil || !receipt.Committed || !receipt.After.Until.IsZero() || receipt.After.UID != seen.UID {
		t.Fatal("actual runtime enable failed", err)
	}
	if err := shared.admin.Get(ctx, key, &cm); err != nil || cm.Data["unrelated"] != "retained" || cm.Annotations["example.test/owner"] != "retained" {
		t.Fatal("runtime update replaced unrelated data", err)
	}
	t.Run("stale-noop", func(t *testing.T) {
		calls := 0
		receipt, err := a.ChangeBlockingRuntime(ctx, time.Time{}, func(context.Context) error {
			calls++
			if calls == 2 {
				if err := shared.admin.Get(ctx, key, &cm); err != nil {
					t.Fatal(err)
				}
				cm.Annotations[blockingUntilKey] = until.Format(time.RFC3339Nano)
				if err := shared.admin.Update(ctx, &cm); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		if !apierrors.IsConflict(err) || receipt.Committed || !receipt.Dispatched || calls != 2 {
			t.Fatal("stale no-op bypassed CAS or retried", err, receipt, calls)
		}
		seen, err := b.ReadBlockingRuntime(ctx)
		if err != nil || !seen.Until.Equal(until) {
			t.Fatal("refusal changed the competing write", err)
		}
	})
	t.Run("final-cancellation", func(t *testing.T) {
		original, stop := context.WithCancel(ctx)
		defer stop()
		calls := 0
		receipt, err := a.ChangeBlockingRuntime(original, time.Time{}, func(context.Context) error {
			calls++
			if calls == 2 {
				stop()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || receipt.Dispatched || receipt.Committed || calls != 2 {
			t.Fatal("canceled original command dispatched", err, receipt)
		}
	})
	t.Run("final-admission-refusal", func(t *testing.T) {
		denied := errors.New("original authorization expired")
		calls := 0
		receipt, err := a.ChangeBlockingRuntime(ctx, time.Time{}, func(context.Context) error {
			calls++
			if calls == 2 {
				return denied
			}
			return nil
		})
		if !errors.Is(err, denied) || receipt.Dispatched || receipt.Committed {
			t.Fatal("final refusal dispatched", err, receipt)
		}
	})
	t.Run("malformed-state", func(t *testing.T) {
		if err := shared.admin.Get(ctx, key, &cm); err != nil {
			t.Fatal(err)
		}
		cm.Annotations[blockingUntilKey] = "untrusted"
		if err := shared.admin.Update(ctx, &cm); err != nil {
			t.Fatal(err)
		}
		if _, err := a.ReadBlockingRuntime(ctx); !errors.Is(err, errBlockingRuntime) {
			t.Fatal("malformed persisted state was treated as enabled", err)
		}
		receipt, err := a.ChangeBlockingRuntime(ctx, time.Time{}, admit)
		if !errors.Is(err, errBlockingRuntime) || receipt.Dispatched {
			t.Fatal("command silently repaired malformed source", err)
		}
	})
}

type blockingLostReply struct {
	next   http.RoundTripper
	writes *atomic.Int32
}

func (r *blockingLostReply) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := r.next.RoundTrip(req)
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/configmaps") {
		r.writes.Add(1)
		if err == nil && response.StatusCode == http.StatusCreated {
			_ = response.Body.Close()
			return nil, errors.New("actual committed reply lost")
		}
	}
	return response, err
}

func TestBlockingRuntimeActualLostReplyNeverReplays(t *testing.T) {
	requireEnv(t)
	ns := blockingNamespace(t)
	cfg := rest.CopyConfig(shared.cfg)
	writes := &atomic.Int32{}
	cfg.Wrap(func(next http.RoundTripper) http.RoundTripper { return &blockingLostReply{next: next, writes: writes} })
	s := newStoreOn(t, cfg, Options{Namespace: ns})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	until := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	receipt, err := s.ChangeBlockingRuntime(ctx, until, func(context.Context) error { return nil })
	if err == nil || !receipt.Dispatched || receipt.Committed || writes.Load() != 1 {
		t.Fatal("lost reply was replayed or claimed a known outcome", err, receipt, writes.Load())
	}
	var persisted corev1.ConfigMap
	if err := shared.admin.Get(ctx, client.ObjectKey{Namespace: ns, Name: BlockingRuntimeName}, &persisted); err != nil || persisted.Annotations[blockingUntilKey] != until.Format(time.RFC3339Nano) {
		t.Fatal("fixture did not lose an actually committed response", err)
	}
}
