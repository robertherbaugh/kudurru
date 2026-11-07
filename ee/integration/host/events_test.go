package host

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/store"
)

type fakeHook struct {
	fakePublisher
	handle func(context.Context, string, integration.Event) error
	calls  int
}

func (*fakeHook) Topics() []string { return []string{"zone.changed"} }
func (p *fakeHook) HandleDelivery(ctx context.Context, id string, e integration.Event) error {
	p.calls++
	return p.handle(ctx, id, e)
}
func TestDurableEventQueueAckAndRestartRetry(t *testing.T) {
	h, j, _, _, now := engineFixture(t)
	id := kinds.Digest([]byte("event"))
	event := integration.Event{Topic: "zone.changed", Time: *now, Payload: json.RawMessage(`{"zone":"example.com"}`)}
	p := &fakeHook{handle: func(_ context.Context, received string, e integration.Event) error {
		d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
		if err != nil || d.Spec.Acknowledged || received != id || string(e.Payload) != string(event.Payload) {
			t.Fatal("callback before durable queue", d, err)
		}
		return &Failure{Kind: FailureRateLimited, RetryAfter: time.Minute}
	}}
	j.provider = p
	a := h.execute(context.Background(), j, command{kind: "deliver", expected: id, event: &event})
	if a.err != nil {
		t.Fatal(a.err)
	}
	d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
	if err != nil || d.Spec.Attempts != 1 || d.Spec.Acknowledged || !d.Spec.NextAttempt.Equal(now.Add(time.Minute)) {
		t.Fatal(d, err)
	}
	// New lease reads the shared durable queue; no callback before its Retry-After.
	j.guard.LeaseID = "restart"
	if err := h.drainEvents(context.Background(), j); err != nil || p.calls != 1 {
		t.Fatal("early event replay", err)
	}
	*now = now.Add(time.Minute)
	p.handle = func(_ context.Context, received string, _ integration.Event) error {
		if received != id {
			t.Fatal("changed retry identity")
		}
		return nil
	}
	if err := h.drainEvents(context.Background(), j); err != nil || p.calls != 2 {
		t.Fatal(err)
	}
	if a := h.execute(context.Background(), j, command{kind: "deliver", expected: id, event: &event}); a.err != nil || p.calls != 2 {
		t.Fatal("acked event repeated", a.err)
	}
	d, err = h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
	if err != nil || !d.Spec.Acknowledged || d.Spec.Attempts != 2 {
		t.Fatal(d, err)
	}
	changed := event
	changed.Payload = json.RawMessage(`{"zone":"foreign"}`)
	if a := h.execute(context.Background(), j, command{kind: "deliver", expected: id, event: &changed}); a.err == nil {
		t.Fatal("ID reused for different payload")
	}
}
func TestEventPermanentExpiryAndSecretRejection(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "permanent"}[permanent], func(t *testing.T) {
			h, j, _, st, now := engineFixture(t)
			p := &fakeHook{handle: func(context.Context, string, integration.Event) error {
				if permanent {
					return &Failure{Kind: FailurePermanent}
				}
				return errors.New("private provider body")
			}}
			j.provider = p
			id := kinds.Digest([]byte("event"))
			event := integration.Event{Topic: "zone.changed", Time: *now, Payload: json.RawMessage(`{"valid":true}`)}
			if a := h.execute(context.Background(), j, command{kind: "deliver", expected: id, event: &event}); a.err != nil {
				t.Fatal(a.err)
			}
			*now = event.Time.Add(24 * time.Hour)
			if err := h.drainEvents(context.Background(), j); err != nil {
				t.Fatal(err)
			}
			d, err := h.opts.Ledger.ReadDelivery(context.Background(), j.guard.Owner, id)
			if err != nil || !d.Spec.DeadLetter || d.Spec.Acknowledged || p.calls != 1 {
				t.Fatal("unbounded/permanent retry", d, err)
			}
			j.frame.instance.secrets["token"] = []byte("secret-value")
			event.Time = *now
			event.Payload = json.RawMessage(`{"token":"s\u0065cret-value"}`)
			if a := h.execute(context.Background(), j, command{kind: "deliver", expected: kinds.Digest([]byte("secret")), event: &event}); !errors.Is(a.err, ErrInvalid) {
				t.Fatal("secret persisted", a.err)
			}
			page, err := st.List(context.Background(), kinds.KindIntegrationDelivery, store.ListOptions{})
			if err != nil || len(page.Items) != 1 {
				t.Fatal(page, err)
			}
		})
	}
}
