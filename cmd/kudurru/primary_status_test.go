package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func primaryStatusDesired(t *testing.T, f *primaryNetworkFixture) *public.DNSZone {
	t.Helper()
	var z public.DNSZone
	if err := f.store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
		t.Fatal(err)
	}
	return &z
}

func primaryStatusObservation(t *testing.T, f *primaryNetworkFixture) pr.PrimaryStatusObservation {
	t.Helper()
	var observation pr.PrimaryStatusObservation
	f.wait(t, func() bool {
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		owner := value.(*servingbuild.RuntimeOwner)
		var ok bool
		observation, ok = owner.Primary().CommittedPrimaryStatus("home")
		return ok && observation.ConfigMatchesDesired && owner.Primary().Admitted("example.", time.Now())
	})
	return observation
}

func TestPrimaryStatusProjectsNativeUpdateAndQuiescesAfterStatusEcho(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	answer, err := f.exchange(primaryUpdateMessage("example.", "dynamic.example.", true), true)
	if err != nil || answer == nil || answer.Rcode != dns.RcodeSuccess {
		t.Fatal("native UPDATE", err)
	}
	original := primaryStatusObservation(t, f)
	status, err := servingbuild.NewPrimaryStatus(servingbuild.PrimaryStatusOptions{Store: f.store, Publisher: f.publisher, Pin: f.keyrings.PrimaryPin(f.publisher)})
	if err != nil {
		t.Fatal(err)
	}
	// Use the actual carried elector context, not a sampled successor token.
	ctx := f.runtime.writer.active.Load().ctx
	wrote, _, err := status.Reconcile(ctx, primaryStatusDesired(t, f))
	if err != nil || !wrote {
		t.Fatal("committed status did not write", wrote, err)
	}
	z := primaryStatusDesired(t, f)
	if z.Status.Serial != original.Serial || z.Status.ContentHash != original.ContentHash || z.Status.RecordCount != original.RecordCount || !meta.IsStatusConditionTrue(z.Status.Conditions, public.ConditionReady) {
		t.Fatal("status did not describe the actual committed data", z.Status)
	}
	// The desired object roster contains no record set for the dynamic answer.
	lease, err := f.publisher.Capture()
	if err != nil {
		t.Fatal(err)
	}
	value, _ := lease.Bundle().Component("objects")
	sets := value.(*zonecache.Snapshot).Objects.List("DNSRecordSet")
	lease.Release()
	if len(sets) != 0 {
		t.Fatal("fixture did not distinguish dynamic from desired records")
	}
	after := primaryStatusObservation(t, f)
	if after.Member.Version != original.Member.Version || after.Serial != original.Serial {
		t.Fatal("status echo rewrote the committed data head")
	}
	for range 8 {
		if wrote, _, err := status.Reconcile(ctx, primaryStatusDesired(t, f)); err != nil || wrote {
			t.Fatal("status failed to quiesce", wrote, err)
		}
	}
	if !equality.Semantic.DeepEqual(z.Status, primaryStatusDesired(t, f).Status) {
		t.Fatal("stable primary status changed timestamps or data")
	}
}

func TestPrimaryStatusHeldProjectionCannotCrossNewNativeHead(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	ctx := f.runtime.writer.active.Load().ctx
	before := primaryStatusDesired(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	base := f.keyrings.PrimaryPin(f.publisher)
	pin := func(ctx context.Context, fence pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		close(entered)
		<-release
		return base(ctx, fence, fn)
	}
	status, err := servingbuild.NewPrimaryStatus(servingbuild.PrimaryStatusOptions{Store: f.store, Publisher: f.publisher, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		wrote bool
		retry time.Duration
		err   error
	}
	done := make(chan result, 1)
	go func() { w, d, e := status.Reconcile(ctx, before); done <- result{w, d, e} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("status never captured original lease")
	}
	answer, updateErr := f.exchange(primaryUpdateMessage("example.", "newer.example.", true), false)
	close(release)
	if updateErr != nil || answer == nil || answer.Rcode != dns.RcodeSuccess {
		t.Fatal("concurrent signed UPDATE failed", updateErr)
	}
	out := <-done
	after := primaryStatusDesired(t, f)
	if out.err != nil || out.wrote || out.retry <= 0 || after.ResourceVersion != before.ResourceVersion || !equality.Semantic.DeepEqual(after.Status, before.Status) {
		t.Fatal("stale head projection wrote public status", out)
	}
}

func TestPrimaryStatusExpiryAtFinalCheckRollsBackAndMissingWriterDenies(t *testing.T) {
	f := newPrimaryNetworkFixture(t)
	ctx := f.runtime.writer.active.Load().ctx
	expires := metav1.NewTime(time.Now().UTC().Add(time.Hour).Truncate(time.Second))
	_, err := f.store.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "expiring", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "expires.example.", RecordType: "A", Targets: []string{"192.0.2.8"}, Kudurru: &public.EndpointExt{ExpiresAt: &expires}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	f.wait(t, func() bool {
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("runtime")
		o, ok := value.(*servingbuild.RuntimeOwner).Primary().CommittedPrimaryStatus("home")
		return ok && !o.ExpiresAt.IsZero() && o.ExpiresAt.Equal(expires.Time)
	})
	before := primaryStatusDesired(t, f)
	var samples atomic.Int32
	now := time.Now()
	status, err := servingbuild.NewPrimaryStatus(servingbuild.PrimaryStatusOptions{Store: f.store, Publisher: f.publisher, Pin: f.keyrings.PrimaryPin(f.publisher), Now: func() time.Time {
		if samples.Add(1) >= 4 {
			return expires.Time
		}
		return now
	}})
	if err != nil {
		t.Fatal(err)
	}
	wrote, retry, err := status.Reconcile(ctx, before)
	if err != nil || wrote || retry <= 0 || samples.Load() < 4 || !equality.Semantic.DeepEqual(before, primaryStatusDesired(t, f)) {
		t.Fatal("elapsed expiry crossed final status commit", wrote, retry, samples.Load(), err)
	}
	if wrote, _, err := status.Reconcile(t.Context(), before); err == nil || wrote {
		t.Fatal("status invented writer authority")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if wrote, _, err := status.Reconcile(canceled, before); !errors.Is(err, context.Canceled) || wrote {
		t.Fatal("status ignored original writer cancellation", err)
	}
}
