package main

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// hookClock shifts the serving runtime's clock, and with it the host's and
// the ledger's, by an offset a test advances. The fixture's license is
// unrestricted and its API token is not used after the shift.
type hookClock struct{ offset atomic.Int64 }

func (c *hookClock) install(o *servingRuntimeOptions) {
	base := o.Now
	if base == nil {
		base = time.Now
	}
	o.Now = func() time.Time { return base().Add(time.Duration(c.offset.Load())) }
}

func (c *hookClock) advance(d time.Duration) { c.offset.Add(int64(d)) }

func (f *hookFixture) delivery(t *testing.T, id string) (kinds.IntegrationDelivery, bool) {
	t.Helper()
	var row kinds.IntegrationDelivery
	err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationDelivery, Name: kinds.DeliveryName(integrationOwner(f.s, f.d), id)}, &row)
	return row, err == nil
}

func (f *hookFixture) readyReason(t *testing.T) string {
	t.Helper()
	var d public.Integration
	if f.s.opts.Store.Get(t.Context(), store.KeyOf(f.d), &d) != nil {
		return ""
	}
	if ready := meta.FindStatusCondition(d.Status.Conditions, "Ready"); ready != nil && ready.Status == metav1.ConditionFalse {
		return ready.Reason
	}
	return ""
}

// A peer that answers 503 for a whole day (D379, F60c-H4): the event is
// retried under its ID until its deadline, then dead-lettered as expired,
// which raises Degraded and publishes it; nothing is sent after that.
func TestIntegrationWebhookHooksMountedExpiryDeadLetters(t *testing.T) {
	p := newHookPeer(t)
	p.setAnswer(func(post hookPost) int {
		if post.topic == "record.changed" {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	clock := &hookClock{}
	f := newHookFixtureWith(t, p, clock.install)
	record := hookApply(t, f.s.opts.Store, hookRecord("expiring")).(*public.DNSRecordSet)
	posts := awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	awaitServingRuntime(t, f.s, func() bool {
		row, ok := f.delivery(t, posts[0].id)
		return ok && row.Spec.ErrorClass == "temporary" && !row.Spec.DeadLetter
	})
	clock.advance(24*time.Hour + time.Minute)
	awaitServingRuntime(t, f.s, func() bool {
		row, ok := f.delivery(t, posts[0].id)
		return ok && row.Spec.DeadLetter && row.Spec.ErrorClass == "expired"
	})
	awaitServingRuntime(t, f.s, func() bool { return f.readyReason(t) == ih.ReasonEventDeadLettered })
	sent := len(p.about("record.changed", string(record.UID)))
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(record.UID))); n != sent {
		t.Fatal("an expired event was sent again", n-sent)
	}
	for _, post := range p.about("record.changed", string(record.UID)) {
		if post.id != posts[0].id || string(post.body) != string(posts[0].body) {
			t.Fatal("a retry changed the delivery")
		}
	}
}

// An exhausted request budget is retried, never dropped (D379, F60c-H4):
// the attempt makes no request, the row stays undelivered with a temporary
// class and its next attempt five minutes out, and once the budget window
// has passed the same event is delivered and acknowledged.
func TestIntegrationWebhookHooksMountedBudgetExhaustionRetries(t *testing.T) {
	p := newHookPeer(t)
	clock := &hookClock{}
	f := newHookFixtureWith(t, p, clock.install)
	owner := integrationOwner(f.s, f.d)
	name := kinds.BudgetName(owner.ClusterID, "webhook", owner.UID)
	// Fill the instance's own budget account (its UID) for this window.
	awaitServingRuntime(t, f.s, func() bool {
		var b kinds.IntegrationBudget
		if f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationBudget, Name: name}, &b) != nil {
			return false
		}
		used := uint32(0)
		for _, r := range b.Spec.Reservations {
			used += r.Cost
		}
		if used >= b.Spec.Capacity {
			return true
		}
		b.Spec.Reservations = append(b.Spec.Reservations, kinds.BudgetReservation{ID: kinds.Digest([]byte("exhaust")), Cost: b.Spec.Capacity - used})
		b.Spec.Sequence++
		_, err := f.s.opts.Store.Apply(t.Context(), &b, store.ApplyOptions{Manager: "integration-ledger", ExpectedResourceVersion: b.ResourceVersion})
		return err == nil
	})
	record := hookApply(t, f.s.opts.Store, hookRecord("budgeted")).(*public.DNSRecordSet)
	id := ih.DeliveryID(owner, ih.ProducedEvent{Topic: "record.changed", Subject: string(record.UID), Version: "1"})
	awaitServingRuntime(t, f.s, func() bool {
		row, ok := f.delivery(t, id)
		return ok && row.Spec.Attempts > 0 && row.Spec.ErrorClass == "temporary" && !row.Spec.DeadLetter && row.Spec.NextAttempt.Sub(f.s.opts.Now()) > 4*time.Minute
	})
	time.Sleep(3 * time.Second)
	if n := len(p.about("record.changed", string(record.UID))); n != 0 {
		t.Fatal("a request was made without budget", n)
	}
	if row, _ := f.delivery(t, id); row.Spec.DeadLetter || row.Spec.Acknowledged {
		t.Fatal("an event refused by the budget was dropped", row.Spec)
	}
	clock.advance(6 * time.Minute)
	posts := awaitPosts(t, f.s, p, "record.changed", string(record.UID), 1)
	if posts[0].id != id {
		t.Fatal("the retried event changed its ID")
	}
	f.signed(t, posts[0])
	awaitServingRuntime(t, f.s, func() bool {
		row, ok := f.delivery(t, id)
		return ok && row.Spec.Acknowledged
	})
}

// A new zone reaches the peer as zone.changed from one source (D379): the
// committed serial of its ZoneState head. The DNSZone object adds no
// generation-versioned event of its own, so every zone.changed of the zone
// names a nonzero serial and is versioned by it.
func TestIntegrationWebhookHooksMountedZoneChangedOneSource(t *testing.T) {
	p := newHookPeer(t)
	f := newHookFixture(t, p)
	zone := hookApply(t, f.s.opts.Store, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "second"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "second.example.", Nameservers: []string{"ns.second.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}}).(*public.DNSZone)
	awaitPosts(t, f.s, p, "zone.changed", string(zone.UID), 1)
	time.Sleep(3 * time.Second)
	owner := integrationOwner(f.s, f.d)
	seen := map[string]bool{}
	for _, post := range p.about("zone.changed", string(zone.UID)) {
		serial, _ := post.payload["serial"].(float64)
		want := ih.DeliveryID(owner, ih.ProducedEvent{Topic: "zone.changed", Subject: string(zone.UID), Version: "s" + strconv.FormatFloat(serial, 'f', 0, 64)})
		if serial == 0 || post.id != want || seen[post.id] {
			t.Fatalf("zone.changed not versioned by one committed serial: id=%s payload=%v", post.id, post.payload)
		}
		seen[post.id] = true
	}
}

// With a small change-feed retention, a quiet kind's position follows the
// head early enough that compaction never passes it while the ticks keep up
// (D379): over more than one compaction no resync happens.
func TestIntegrationWebhookHooksMountedQuietKindSurvivesCompaction(t *testing.T) {
	p := newHookPeer(t)
	f := newHookFixture(t, p)
	owner := integrationOwner(f.s, f.d)
	f.feedRetention = 256
	f.restart(t, nil)
	cursor := func() kinds.IntegrationEventCursor {
		var c kinds.IntegrationEventCursor
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: kinds.KindIntegrationEventCursor, Name: kinds.EventCursorName(owner)}, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	group := hookApply(t, f.s.opts.Store, &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "churn", Namespace: "default"}}).(*public.ClientGroup)
	// Batches of writes of an unwatched kind, each well inside the
	// retention, with a production tick after each.
	for batch := range 12 {
		before := cursor().ResourceVersion
		for i := range 100 {
			group.Labels = map[string]string{"churn": strconv.Itoa(batch*100 + i)}
			group = hookApply(t, f.s.opts.Store, group).(*public.ClientGroup)
		}
		// Let a production tick follow the head: the cursor changes once
		// the quiet kinds' positions move. (A fixed 1,024 distance would
		// never move them here, and the wait just runs out.)
		for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline) && cursor().ResourceVersion == before; {
			time.Sleep(50 * time.Millisecond)
		}
		if _, resync := cursor().Spec.Positions[positionResync]; resync {
			t.Fatal("a quiet kind was compacted past", batch)
		}
	}
	if f.s.opts.Store.FeedCompacted() == 0 {
		t.Fatal("the feed was not compacted")
	}
	for _, post := range p.topic("zone.changed") {
		if post.payload["change"] == "resync" {
			t.Fatal("a resync happened while the ticks kept up", post.payload)
		}
	}
}
