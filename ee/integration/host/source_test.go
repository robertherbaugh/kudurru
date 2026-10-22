package host

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/inventory"
)

type fakeObserver struct {
	fakePublisher
	observe func(context.Context) (SourceSnapshot, error)
}

func (p *fakeObserver) Observe(ctx context.Context) (SourceSnapshot, error) { return p.observe(ctx) }
func TestObservationAbsoluteExpiryAndFailedPollRetention(t *testing.T) {
	h, j, _, _, now := engineFixture(t)
	h.opts.Inventory = inventory.New(func() time.Time { return *now })
	source := SourceID(j.guard.Owner)
	observed := *now
	expires := observed.Add(3 * time.Minute)
	ip := netip.MustParseAddr("192.0.2.1")
	clients := []inventory.Client{{ID: "device", IPs: []netip.Addr{ip}, Hostname: "device.example", ObservedAt: observed, ExpiresAt: expires}}
	p := &fakeObserver{observe: func(context.Context) (SourceSnapshot, error) {
		return SourceSnapshot{Clients: clients, Complete: true}, nil
	}}
	j.provider = p
	if a := h.execute(context.Background(), j, command{kind: "sync"}); a.err != nil {
		t.Fatal(a.err)
	}
	if got := h.opts.Inventory.Snapshot().Lookup(ip, *now); got == nil || got.Source != source {
		t.Fatal("observation missing", got)
	}
	*now = now.Add(time.Minute)
	if a := h.execute(context.Background(), j, command{kind: "sync"}); a.err != nil {
		t.Fatal(a.err)
	}
	if got := h.opts.Inventory.Snapshot().Lookup(ip, *now); got == nil || !got.ExpiresAt.Equal(expires) {
		t.Fatal("poll renewed stale LastSeen", got)
	}
	p.observe = func(context.Context) (SourceSnapshot, error) {
		return SourceSnapshot{}, errors.New("private auth body")
	}
	if a := h.execute(context.Background(), j, command{kind: "sync"}); !errors.Is(a.err, ErrProvider) {
		t.Fatal(a.err)
	}
	if h.opts.Inventory.Snapshot().Lookup(ip, expires) != nil {
		t.Fatal("expired inventory survived exact boundary")
	}
}
func TestMalformedIncompleteAndFencedObservationsDoNotPublish(t *testing.T) {
	h, j, _, _, now := engineFixture(t)
	h.opts.Inventory = inventory.New(func() time.Time { return *now })
	published := 0
	h.opts.Publish = func(context.Context, ExecutionGuard, DesiredBatch) error { published++; return nil }
	p := &fakeObserver{observe: func(context.Context) (SourceSnapshot, error) {
		return SourceSnapshot{Desired: &DesiredBatch{}, Complete: false}, nil
	}}
	j.provider = p
	if a := h.execute(context.Background(), j, command{kind: "sync"}); !errors.Is(a.err, ErrInvalid) || published != 0 {
		t.Fatal(a.err)
	}
	p.observe = func(context.Context) (SourceSnapshot, error) {
		return SourceSnapshot{Desired: &DesiredBatch{}, Clients: []inventory.Client{{ID: "malformed"}}, Complete: true}, nil
	}
	if a := h.execute(context.Background(), j, command{kind: "sync"}); !errors.Is(a.err, ErrInvalid) || published != 0 {
		t.Fatal("malformed poll authorized deletes", a.err)
	}
	p.observe = func(context.Context) (SourceSnapshot, error) {
		h.mu.Lock()
		j.frame.input.BundleID = "new"
		h.mu.Unlock()
		return SourceSnapshot{Desired: &DesiredBatch{}, Complete: true}, nil
	}
	if a := h.execute(context.Background(), j, command{kind: "sync"}); !errors.Is(a.err, ErrFenced) || published != 0 || h.opts.Inventory.Snapshot() != nil {
		t.Fatal("old poll published into new capture", a.err)
	}
}
func TestCompleteEmptyObservationRemovesOwnedInventory(t *testing.T) {
	h, j, _, _, now := engineFixture(t)
	h.opts.Inventory = inventory.New(func() time.Time { return *now })
	p := &fakeObserver{observe: func(context.Context) (SourceSnapshot, error) {
		return SourceSnapshot{Complete: true, Desired: &DesiredBatch{SourceID: "foreign"}}, nil
	}}
	j.provider = p
	h.opts.Publish = func(_ context.Context, g ExecutionGuard, b DesiredBatch) error {
		if b.SourceID != SourceID(g.Ledger.Owner) {
			t.Fatal("provider loaned foreign source identity")
		}
		return nil
	}
	ip := netip.MustParseAddr("192.0.2.1")
	if err := h.opts.Inventory.Replace(SourceID(j.guard.Owner), 1, []inventory.Client{{ID: "old", IPs: []netip.Addr{ip}, ObservedAt: *now, ExpiresAt: now.Add(time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if a := h.execute(context.Background(), j, command{kind: "sync"}); a.err != nil {
		t.Fatal(a.err)
	}
	if h.opts.Inventory.Snapshot().Lookup(ip, *now) != nil {
		t.Fatal("complete empty poll retained removed device")
	}
}
func TestRetryClassificationBounds(t *testing.T) {
	h, _, _, _, _ := engineFixture(t)
	if _, retry := h.retryDelay(&Failure{Kind: FailurePermanent}, 1); retry {
		t.Fatal("permanent response retried")
	}
	if delay, retry := h.retryDelay(&Failure{Kind: FailureRateLimited, RetryAfter: 12 * time.Minute}, 1); !retry || delay != 12*time.Minute {
		t.Fatal("Retry-After ignored", delay, retry)
	}
	for _, count := range []uint32{1, 10, 32} {
		delay, retry := h.retryDelay(errors.New("transport"), count)
		if !retry || delay < time.Second || delay > 375*time.Second {
			t.Fatal("unbounded backoff", delay)
		}
	}
	if retryFailure(&Failure{Kind: FailureRateLimited, RetryAfter: 24 * time.Hour}) != nil {
		t.Fatal("unbounded provider Retry-After accepted")
	}
}

func TestCombinedPublisherObserverUsesOnePlanAndOnePoll(t *testing.T) {
	h, j, p, _, now := engineFixture(t)
	h.opts.Inventory = inventory.New(func() time.Time { return *now })
	polls := 0
	observer := &fakeObserver{fakePublisher: *p, observe: func(context.Context) (SourceSnapshot, error) {
		polls++
		return SourceSnapshot{Complete: true, Clients: []inventory.Client{{ID: "device", ObservedAt: *now, ExpiresAt: now.Add(time.Minute)}}}, nil
	}}
	j.provider = observer
	f, g, _ := h.captured(j)
	preview, err := h.preview(context.Background(), j, f, g)
	if err != nil {
		t.Fatal(err)
	}
	a := h.execute(context.Background(), j, command{kind: "sync", expected: preview.Digest})
	if a.err != nil || a.result.Applied != 1 || a.result.Clients != 1 || observer.calls != 1 || polls != 1 {
		t.Fatal("combined capabilities skipped/repeated work", a.result, polls, observer.calls, a.err)
	}
}
