package host

import (
	"context"
	"errors"
	"slices"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
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
	if roles, err := providerRoles(j.provider); err != nil || !roles.Hook {
		ok = false
	}
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
	out, _, err := h.allDeliveries(ctx, owner)
	return out, err
}

// allDeliveries returns this owner's rows ordered by next attempt, and the
// rows of other owners whose deadline passed long enough ago to be pruned.
func (h *Host) allDeliveries(ctx context.Context, owner InstanceID) ([]*kinds.IntegrationDelivery, []*kinds.IntegrationDelivery, error) {
	var out, others []*kinds.IntegrationDelivery
	now := h.opts.Now()
	continuation := ""
	seen := map[string]bool{}
	count := 0
	for {
		page, err := h.opts.Reader.List(ctx, kinds.KindIntegrationDelivery, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			return nil, nil, err
		}
		for _, item := range page.Items {
			count++
			if count > 100000 {
				return nil, nil, ErrCapacity
			}
			d, ok := item.(*kinds.IntegrationDelivery)
			if !ok || d == nil || d.Validate() != nil || d.ResourceVersion == "" {
				return nil, nil, ErrInvalid
			}
			if seen[d.Name] {
				return nil, nil, ErrInvalid
			}
			seen[d.Name] = true
			if d.Spec.Owner == owner {
				out = append(out, d.DeepCopy())
			} else if len(others) < maxPrunePerTick && !now.Before(d.Spec.Deadline.Add(deliveryRetention)) {
				others = append(others, d.DeepCopy())
			}
		}
		if page.Continue == "" {
			break
		}
		if seen["page:"+page.Continue] || page.Continue == continuation {
			return nil, nil, ErrInvalid
		}
		seen["page:"+page.Continue] = true
		continuation = page.Continue
	}
	slices.SortFunc(out, func(a, b *kinds.IntegrationDelivery) int { return a.Spec.NextAttempt.Compare(b.Spec.NextAttempt) })
	return out, others, nil
}
func (h *Host) drainEvents(ctx context.Context, j *job) error {
	f, g, err := h.captured(j)
	if err != nil {
		return err
	}
	ctx, err = h.scopedOriginal(ctx, j, f, g)
	if err != nil {
		return err
	}
	queued, others, err := h.allDeliveries(ctx, g.Ledger.Owner)
	if err != nil {
		return err
	}
	// Retention first: the 100,000-row listing bound must never be reached
	// by rows that can no longer be delivered (D379).
	// A refused prune (a row changed under it, a store without deletes)
	// never holds back delivery; it is retried on the next tick.
	if err := h.pruneDeliveries(ctx, j, f, g, append(slices.Clone(queued), others...)); err != nil {
		if errors.Is(err, ErrFenced) || ctx.Err() != nil {
			return err
		}
		h.opts.Logger.Warn("integration delivery retention failed", "integration", j.key)
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

// deliveryRetention is how long a row stays after its deadline. Past the
// deadline no queue can recreate its ID (queueEvent refuses an event older
// than a day), so pruning it cannot turn into a duplicate delivery (D379).
const deliveryRetention = ledger.DeliveryRetention

// maxPrunePerTick bounds the exact-version deletes of one drain.
const maxPrunePerTick = 64

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
		if _, err := h.opts.Ledger.SaveDelivery(ctx, g.Ledger, next); err != nil {
			return err
		}
		h.deadLettered(ctx, j, g)
		return nil
	}
	hook, ok := j.provider.(DeliveryHook)
	if roles, err := providerRoles(j.provider); err != nil || !roles.Hook {
		ok = false
	}
	if !ok {
		return ErrUnsupported
	}
	next.Spec.Attempts++
	err := hook.HandleDelivery(ctx, next.Spec.DeliveryID, integration.Event{Topic: next.Spec.Topic, Time: next.Spec.CreatedAt, Payload: slices.Clone(next.Spec.Payload)})
	if err != nil && localRefusal(ctx, err) {
		// The attempt never reached a peer answer: this lease lost its
		// authority, the provider stopped or the caller's lifetime ended.
		// Nothing about the event is decided; the next lease retries it.
		return err
	}
	if errors.Is(err, ledger.ErrBudget) {
		err = &Failure{Kind: FailureRateLimited, RetryAfter: 5 * time.Minute}
	}
	dead := false
	if err == nil {
		next.Spec.Acknowledged = true
		next.Spec.ErrorClass = ""
	} else {
		delay, retry := h.retryDelay(err, next.Spec.Attempts)
		if !retry {
			next.Spec.DeadLetter = true
			next.Spec.ErrorClass = "permanent"
			dead = true
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
	if _, saveErr := h.opts.Ledger.SaveDelivery(ctx, g.Ledger, next); saveErr != nil {
		return saveErr
	}
	if dead {
		h.deadLettered(ctx, j, g)
	}
	return nil
}

// localRefusal is an attempt that ended on this side: a fence, a stopped
// provider, or the end of the caller's own lifetime.
func localRefusal(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, ErrFenced) || errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// deadLettered raises Degraded for a dropped event and publishes it (D379).
// The counts of the last sync stay; the next successful sync clears it.
func (h *Host) deadLettered(ctx context.Context, j *job, g ExecutionGuard) {
	h.mu.RLock()
	old := h.statuses[j.key]
	result := valueResult(&old.Result)
	h.mu.RUnlock()
	h.setLocal(j, "Degraded", ReasonEventDeadLettered, result, true)
	h.publishStatus(ctx, j, g)
}

// ReasonEventDeadLettered is the status reason of an instance that dropped
// an event: the peer refused it with a 4xx answer, or its day ran out.
const ReasonEventDeadLettered = "EventDeadLettered"

// pruneDeliveries deletes, by exact UID and resource version, rows whose
// deadline passed more than deliveryRetention ago: this instance's terminal
// rows, and the rows of an instance that no longer exists in the captured
// cut. Live owners other than this one are left to their own lease.
func (h *Host) pruneDeliveries(ctx context.Context, j *job, f frame, g ExecutionGuard, rows []*kinds.IntegrationDelivery) error {
	if len(rows) == 0 {
		return nil
	}
	live := map[string]bool{}
	for _, obj := range f.input.Objects.List("Integration") {
		live[string(obj.GetUID())] = true
	}
	now := h.opts.Now()
	// Own and foreign rows are separate calls, so a foreign row that changed
	// under the listing cannot hold back this instance's own retention. The
	// ledger itself skips a foreign row whose owner still exists.
	var own, foreign []*kinds.IntegrationDelivery
	for _, d := range rows {
		if now.Before(d.Spec.Deadline.Add(deliveryRetention)) {
			continue
		}
		switch {
		case d.Spec.Owner == g.Ledger.Owner:
			if (d.Spec.Acknowledged || d.Spec.DeadLetter) && len(own) < maxPrunePerTick {
				own = append(own, d)
			}
		case f.input.Complete && !live[d.Spec.Owner.UID] && len(foreign) < maxPrunePerTick:
			foreign = append(foreign, d)
		}
	}
	var failed error
	for _, batch := range [][]*kinds.IntegrationDelivery{own, foreign} {
		if len(batch) == 0 {
			continue
		}
		if err := h.fence(ctx, j, g); err != nil {
			return err
		}
		if err := h.opts.Ledger.PruneDeliveries(ctx, g.Ledger, batch); err != nil {
			failed = err
		}
	}
	return failed
}
