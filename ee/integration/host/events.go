package host

import (
	"context"
	"slices"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DeliveryHook requires the stable ID needed for at-least-once deduplication.
// Tier-B providers sign the original body and send this ID on every retry.
type DeliveryHook interface {
	Topics() []string
	HandleDelivery(context.Context, string, integration.Event) error
}

// Deliver queues a sanitized event durably before its first callback. Root owns
// event subscription authorization/redaction; resolved named secrets are rejected.
func (h *Host) Deliver(ctx context.Context, id InstanceID, deliveryID string, event integration.Event) error {
	if !kinds.ValidDigest(deliveryID) || event.Time.IsZero() || len(event.Payload) > kinds.MaxPayloadBytes {
		return ErrInvalid
	}
	event.Payload = slices.Clone(event.Payload)
	return h.submit(ctx, id, command{kind: "deliver", expected: deliveryID, event: &event}).err
}
func (h *Host) queueEvent(ctx context.Context, j *job, f frame, g ExecutionGuard, c command) error {
	hook, ok := j.provider.(DeliveryHook)
	if !ok {
		return ErrUnsupported
	}
	if c.event == nil || containsSecret(c.event.Payload, f.instance.secrets) {
		return ErrInvalid
	}
	topics := hook.Topics()
	if len(topics) > 128 || !slices.Contains(topics, c.event.Topic) {
		return ErrUnsupported
	}
	now := h.opts.Now().UTC()
	created := c.event.Time.UTC()
	deadline := created.Add(24 * time.Hour)
	if created.After(now) || !now.Before(deadline) {
		return ErrInvalid
	}
	queued, err := h.deliveries(ctx, g.Ledger.Owner)
	if err != nil {
		return err
	}
	existing := false
	pending := 0
	for _, d := range queued {
		existing = existing || d.Spec.DeliveryID == c.expected
		if !d.Spec.Acknowledged && !d.Spec.DeadLetter {
			pending++
		}
	}
	if !existing && pending >= 256 {
		return ErrCapacity
	}
	d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(g.Ledger.Owner, c.expected)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: g.Ledger.Owner, DeliveryID: c.expected, Topic: c.event.Topic, Payload: slices.Clone(c.event.Payload), CreatedAt: created, Deadline: deadline, NextAttempt: created}}
	saved, err := h.opts.Ledger.QueueDelivery(ctx, g.Ledger, d)
	if err != nil {
		return err
	}
	return h.deliver(ctx, j, g, saved)
}
func (h *Host) deliveries(ctx context.Context, owner InstanceID) ([]*kinds.IntegrationDelivery, error) {
	var out []*kinds.IntegrationDelivery
	continuation := ""
	seen := map[string]bool{}
	count := 0
	for {
		page, err := h.opts.Reader.List(ctx, kinds.KindIntegrationDelivery, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			count++
			if count > 100000 {
				return nil, ErrCapacity
			}
			d, ok := item.(*kinds.IntegrationDelivery)
			if !ok || d == nil || d.Validate() != nil || d.ResourceVersion == "" {
				return nil, ErrInvalid
			}
			if d.Spec.Owner == owner {
				if seen[d.Name] {
					return nil, ErrInvalid
				}
				seen[d.Name] = true
				out = append(out, d.DeepCopy())
			}
		}
		if page.Continue == "" {
			break
		}
		if seen["page:"+page.Continue] || page.Continue == continuation {
			return nil, ErrInvalid
		}
		seen["page:"+page.Continue] = true
		continuation = page.Continue
	}
	slices.SortFunc(out, func(a, b *kinds.IntegrationDelivery) int { return a.Spec.NextAttempt.Compare(b.Spec.NextAttempt) })
	return out, nil
}
func (h *Host) drainEvents(ctx context.Context, j *job) error {
	_, g, err := h.captured(j)
	if err != nil {
		return err
	}
	ctx = scoped(ctx, j, g)
	queued, err := h.deliveries(ctx, g.Ledger.Owner)
	if err != nil {
		return err
	}
	attempted := 0
	for _, d := range queued {
		if d.Spec.Acknowledged || d.Spec.DeadLetter || h.opts.Now().Before(d.Spec.NextAttempt) {
			continue
		}
		if attempted == 64 {
			break
		}
		attempted++
		if err := h.deliver(ctx, j, g, d); err != nil {
			return err
		}
	}
	return nil
}
func (h *Host) deliver(ctx context.Context, j *job, g ExecutionGuard, d *kinds.IntegrationDelivery) error {
	if d.Spec.Acknowledged || d.Spec.DeadLetter || h.opts.Now().Before(d.Spec.NextAttempt) {
		return nil
	}
	if err := h.fence(ctx, j, g); err != nil {
		return err
	}
	next := d.DeepCopy()
	now := h.opts.Now().UTC()
	if !now.Before(next.Spec.Deadline) || next.Spec.Attempts >= 10000 {
		next.Spec.DeadLetter = true
		next.Spec.ErrorClass = "expired"
		next.Spec.NextAttempt = next.Spec.Deadline
		_, err := h.opts.Ledger.SaveDelivery(ctx, g.Ledger, next)
		return err
	}
	hook, ok := j.provider.(DeliveryHook)
	if !ok {
		return ErrUnsupported
	}
	next.Spec.Attempts++
	err := hook.HandleDelivery(ctx, next.Spec.DeliveryID, integration.Event{Topic: next.Spec.Topic, Time: next.Spec.CreatedAt, Payload: slices.Clone(next.Spec.Payload)})
	if err == nil {
		next.Spec.Acknowledged = true
		next.Spec.ErrorClass = ""
	} else {
		delay, retry := h.retryDelay(err, next.Spec.Attempts)
		if !retry {
			next.Spec.DeadLetter = true
			next.Spec.ErrorClass = "permanent"
		} else {
			next.Spec.ErrorClass = "temporary"
			next.Spec.NextAttempt = now.Add(delay)
			if next.Spec.NextAttempt.After(next.Spec.Deadline) {
				next.Spec.NextAttempt = next.Spec.Deadline
			}
		}
	}
	// Failure to save the acknowledgment deliberately leaves a retriable event.
	// Providers deduplicate stable IDs; there is no exactly-once promise.
	_, saveErr := h.opts.Ledger.SaveDelivery(ctx, g.Ledger, next)
	return saveErr
}
