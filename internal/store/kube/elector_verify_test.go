package kube

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/robertherbaugh/kudurru/internal/store"
)

// TestLeaseTermVerifyRefusals: a recorded term verifies only while the Lease
// still names this identity with the recorded acquisition and an unexpired
// renew time; any change, an API error or a canceled context refuses (D384).
func TestLeaseTermVerifyRefusals(t *testing.T) {
	acquired := metav1.NewMicroTime(time.Now().Add(-time.Second).Truncate(time.Microsecond))
	lease := func() *coordinationv1.Lease {
		renew := metav1.NewMicroTime(time.Now())
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "kudurru-integration"}, Spec: coordinationv1.LeaseSpec{
			HolderIdentity: ptr.To("pod-a"), AcquireTime: &acquired, RenewTime: &renew, LeaseDurationSeconds: ptr.To[int32](15), LeaseTransitions: ptr.To[int32](3)}}
	}
	for _, tc := range []struct {
		name   string
		change func(*coordinationv1.Lease)
		fail   bool
		cancel bool
		term   bool
		ok     bool
	}{
		{name: "current", ok: true},
		{name: "holder changed", change: func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = ptr.To("pod-b") }},
		{name: "holder released", change: func(l *coordinationv1.Lease) { l.Spec.HolderIdentity = ptr.To("") }},
		{name: "acquisition changed", change: func(l *coordinationv1.Lease) {
			again := metav1.NewMicroTime(acquired.Add(time.Second))
			l.Spec.AcquireTime = &again
		}},
		{name: "transitions changed", change: func(l *coordinationv1.Lease) { l.Spec.LeaseTransitions = ptr.To[int32](4) }},
		{name: "expired", change: func(l *coordinationv1.Lease) {
			old := metav1.NewMicroTime(time.Now().Add(-time.Minute))
			l.Spec.RenewTime = &old
		}},
		{name: "api error", fail: true},
		{name: "canceled context", cancel: true},
		{name: "term ended", term: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset(lease())
			e, err := NewLeaseElector(client.CoordinationV1(), ElectorConfig{Namespace: "ns", Name: "kudurru-integration", Identity: "pod-a", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			termCtx, endTerm := context.WithCancel(context.Background())
			defer endTerm()
			proof := e.recordTerm(termCtx)
			if !proof.recorded {
				t.Fatal("acquisition was not recorded")
			}
			term := context.WithValue(store.WithTermVerifier(termCtx, proof.verify), leaseTermKey{}, e)
			if tc.change != nil {
				l := lease()
				tc.change(l)
				if _, err := client.CoordinationV1().Leases("ns").Update(context.Background(), l, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fail {
				client.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("apiserver unavailable")
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if tc.term {
				endTerm()
			}
			err = e.VerifyTerm(ctx, term)
			if tc.ok != (err == nil) {
				t.Fatalf("VerifyTerm = %v, want ok=%t", err, tc.ok)
			}
			if err != nil && !errors.Is(err, store.ErrTermLost) {
				t.Fatal("refusal is not ErrTermLost", err)
			}
			// Verification is read-only: it never renewed or rewrote the Lease.
			for _, a := range client.Actions() {
				if a.GetVerb() != "get" && a.GetVerb() != "update" || a.GetVerb() == "update" && tc.change == nil {
					t.Fatalf("unexpected lease %s", a.GetVerb())
				}
			}
		})
	}
	// A term context from another elector, or none, never verifies.
	client := fake.NewClientset(lease())
	e, _ := NewLeaseElector(client.CoordinationV1(), ElectorConfig{Namespace: "ns", Name: "kudurru-integration", Identity: "pod-a", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second})
	other, _ := NewLeaseElector(client.CoordinationV1(), ElectorConfig{Namespace: "ns", Name: "kudurru-integration", Identity: "pod-a", LeaseDuration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second})
	proof := other.recordTerm(context.Background())
	foreign := context.WithValue(store.WithTermVerifier(context.Background(), proof.verify), leaseTermKey{}, other)
	if err := e.VerifyTerm(context.Background(), foreign); !errors.Is(err, store.ErrTermLost) {
		t.Fatal("foreign term verified", err)
	}
	if err := store.VerifyTerm(context.Background(), context.Background()); !errors.Is(err, store.ErrTermLost) {
		t.Fatal("context without a term verified", err)
	}
}

// TestLeaseElectorVerifyTermAgainstSuccessor (envtest): while the first
// elector still believes it leads, a successor's acquisition or a partition
// makes its VerifyTerm fail before its term context is canceled.
func TestLeaseElectorVerifyTermAgainstSuccessor(t *testing.T) {
	requireEnv(t)
	lease := uniqueLease()
	// A long retry period keeps pod-a's local belief well past the takeover.
	cb := newCandidate(t, lease, "pod-a", 20*time.Second, 15*time.Second, 5*time.Second)
	var mu sync.Mutex
	var termCtx context.Context
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("elector did not return")
		}
	})
	started := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = cb.e.Run(ctx, func(term context.Context) {
			mu.Lock()
			termCtx = term
			mu.Unlock()
			close(started)
			<-term.Done()
		}, nil)
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("no term")
	}
	mu.Lock()
	term := termCtx
	mu.Unlock()
	if err := cb.e.VerifyTerm(ctx, term); err != nil {
		t.Fatal("current term did not verify", err)
	}
	// Partition: the verification read fails while the term is still live.
	cb.down.Store(true)
	if err := cb.e.VerifyTerm(ctx, term); !errors.Is(err, store.ErrTermLost) || term.Err() != nil {
		t.Fatal("partitioned term verified or already ended", err, term.Err())
	}
	cb.down.Store(false)
	if err := cb.e.VerifyTerm(ctx, term); err != nil {
		t.Fatal("healed term did not verify", err)
	}
	// A successor acquires the Lease (as after expiry): the first elector's
	// term fails verification before its own renew loop notices.
	leases := leaseClient(t)
	if err := retryOnConflict(func() error {
		l, err := leases.Get(ctx, lease, metav1.GetOptions{})
		if err != nil {
			return err
		}
		now := metav1.NewMicroTime(time.Now())
		l.Spec.HolderIdentity = ptr.To("pod-b")
		l.Spec.AcquireTime, l.Spec.RenewTime = &now, &now
		l.Spec.LeaseTransitions = ptr.To(ptr.Deref(l.Spec.LeaseTransitions, 0) + 1)
		_, err = leases.Update(ctx, l, metav1.UpdateOptions{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := cb.e.VerifyTerm(ctx, term); !errors.Is(err, store.ErrTermLost) {
		t.Fatal("term verified after a successor acquired", err)
	}
	if term.Err() != nil {
		t.Fatal("the term ended before verification could refuse; the test proves nothing")
	}
}
