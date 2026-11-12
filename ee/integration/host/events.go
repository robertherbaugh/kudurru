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
	if !existing && (pending >= maxPendingDeliveries || len(queued) >= maxInstanceDeliveries) {
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

// maxOwnDeliveries bounds the rows of one owner a listing keeps in memory.
// The production and queue budgets keep an instance at
// maxInstanceDeliveries rows, far below it.
const maxOwnDeliveries = 100000

// allDeliveries returns this owner's rows ordered by next attempt, and at
// most maxPrunePerTick rows of other owners that retention may delete: any
// row past its deadline plus deliveryRetention, terminal or not, whatever
// its owner. Past the deadline no queue accepts its event again, and an
// owner that runs any lease has dead-lettered its own undelivered rows by
// then (D379, D380). The scan pages through every row and keeps only these,
// so its memory is bounded by this owner's rows however many rows other
// owners hold, and retention keeps working above any listing size.
func (h *Host) allDeliveries(ctx context.Context, owner InstanceID) ([]*kinds.IntegrationDelivery, []*kinds.IntegrationDelivery, error) {
	var out, others []*kinds.IntegrationDelivery
	now := h.opts.Now()
	continuation := ""
	seen := map[string]bool{}
	for {
		page, err := h.opts.Reader.List(ctx, kinds.KindIntegrationDelivery, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			return nil, nil, err
		}
		for _, item := range page.Items {
			d, ok := item.(*kinds.IntegrationDelivery)
			if !ok || d == nil || d.Validate() != nil || d.ResourceVersion == "" {
				return nil, nil, ErrInvalid
			}
			if seen[d.Name] {
				return nil, nil, ErrInvalid
			}
			seen[d.Name] = true
			if d.Spec.Owner == owner {
				if len(out) == maxOwnDeliveries {
					return nil, nil, ErrCapacity
				}
				out = append(out, d.DeepCopy())
				continue
			}
			if len(others) < maxPrunePerTick && !now.Before(d.Spec.Deadline.Add(deliveryRetention)) {
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

// retainEvents runs delivery and cursor retention on one elected lease and
// returns the lease's own rows. Retention runs first, so rows that can no
// longer be delivered never hold back production (D379). A refused prune (a
// row changed under it, a store without deletes) never holds back delivery;
// it is retried on the next tick.
func (h *Host) retainEvents(ctx context.Context, j *job, f frame, g ExecutionGuard) ([]*kinds.IntegrationDelivery, error) {
	queued, others, err := h.allDeliveries(ctx, g.Ledger.Owner)
	if err != nil {
		return nil, err
	}
	if err := h.pruneDeliveries(ctx, j, g, queued, others); err != nil {
		if errors.Is(err, ErrFenced) || ctx.Err() != nil {
			return nil, err
		}
		h.opts.Logger.Warn("integration delivery retention failed", "integration", j.key)
	}
	if f.input.Complete {
		live := map[string]bool{}
		for _, obj := range f.input.Objects.List("Integration") {
			live[string(obj.GetUID())] = true
		}
		if err := h.pruneEventCursors(ctx, j, g, live); err != nil {
			if errors.Is(err, ErrFenced) || ctx.Err() != nil {
				return nil, err
			}
			h.opts.Logger.Warn("integration event cursor retention failed", "integration", j.key)
		}
	}
	return queued, nil
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
	queued, err := h.retainEvents(ctx, j, f, g)
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

// retentionTicks is how many poll intervals pass between the retention runs
// of a lease that receives no events (10 minutes at the default 2 s). Such a
// lease delivers nothing, but it still prunes what other instances left and
// dead-letters its own undelivered rows once their day ran out, as when its
// peer stopped advertising hooks. So rows are pruned while any Integration
// runs a lease, not only while one receives events (D379, D380).
const retentionTicks = 300

// expireEvents is the retention run of a lease without the hook role: it
// prunes like a hook lease and dead-letters, never attempts, its own rows
// whose deadline passed.
func (h *Host) expireEvents(ctx context.Context, j *job) error {
	f, g, err := h.captured(j)
	if err != nil {
		return err
	}
	ctx, err = h.scopedOriginal(ctx, j, f, g)
	if err != nil {
		return err
	}
	queued, err := h.retainEvents(ctx, j, f, g)
	if err != nil {
		return err
	}
	expired := 0
	for _, d := range queued {
		if d.Spec.Acknowledged || d.Spec.DeadLetter || h.opts.Now().Before(d.Spec.Deadline) {
			continue
		}
		if expired == maxPrunePerTick {
			break
		}
		expired++
		// deliver dead-letters a row past its deadline before it asks for
		// the hook role, so this never reaches a peer.
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

// pruneDeliveries deletes rows by exact UID and resource version, at most
// maxPrunePerTick of each kind per tick (D379):
//
//   - this instance's own terminal rows, oldest first: those past their
//     deadline plus deliveryRetention, and as many more as the instance
//     holds above maxTerminalDeliveries, so its rows stay bounded by count
//     even when a prompt peer acknowledges everything;
//   - the rows of other owners allDeliveries selected: every row past its
//     retention, terminal or not, whether or not its owner still exists.
func (h *Host) pruneDeliveries(ctx context.Context, j *job, g ExecutionGuard, own, foreign []*kinds.IntegrationDelivery) error {
	now := h.opts.Now()
	var terminal []*kinds.IntegrationDelivery
	for _, d := range own {
		if d.Spec.Acknowledged || d.Spec.DeadLetter {
			terminal = append(terminal, d)
		}
	}
	slices.SortFunc(terminal, func(a, b *kinds.IntegrationDelivery) int { return a.Spec.CreatedAt.Compare(b.Spec.CreatedAt) })
	evict := max(len(terminal)-maxTerminalDeliveries, 0)
	for evict < len(terminal) && !now.Before(terminal[evict].Spec.Deadline.Add(deliveryRetention)) {
		evict++
	}
	terminal = terminal[:min(evict, maxPrunePerTick)]
	var failed error
	if len(terminal) > 0 {
		if err := h.fence(ctx, j, g); err != nil {
			return err
		}
		if err := h.opts.Ledger.EvictDeliveries(ctx, g.Ledger, terminal); err != nil {
			failed = err
		}
	}
	// Separate calls, so a foreign row that changed under the listing
	// cannot hold back this instance's own retention.
	if len(foreign) > 0 {
		if err := h.fence(ctx, j, g); err != nil {
			return err
		}
		if err := h.opts.Ledger.PruneDeliveries(ctx, g.Ledger, foreign); err != nil {
			failed = err
		}
	}
	return failed
}

// pruneEventCursors deletes, by exact UID and resource version, at most
// maxPrunePerTick event cursors whose owner is absent from the complete
// captured cut, such as the cursor of an instance deleted under the Retain
// policy (D379). The ledger itself skips a cursor whose owner still exists.
func (h *Host) pruneEventCursors(ctx context.Context, j *job, g ExecutionGuard, live map[string]bool) error {
	var orphans []*kinds.IntegrationEventCursor
	continuation := ""
	seen := map[string]bool{}
	for len(orphans) < maxPrunePerTick {
		page, err := h.opts.Reader.List(ctx, kinds.KindIntegrationEventCursor, store.ListOptions{Limit: 256, Continue: continuation})
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			c, ok := item.(*kinds.IntegrationEventCursor)
			if !ok || c == nil || c.Validate() != nil || c.ResourceVersion == "" || seen[c.Name] {
				return ErrInvalid
			}
			seen[c.Name] = true
			if c.Spec.Owner != g.Ledger.Owner && !live[c.Spec.Owner.UID] && len(orphans) < maxPrunePerTick {
				orphans = append(orphans, c.DeepCopy())
			}
		}
		if page.Continue == "" {
			break
		}
		if seen["page:"+page.Continue] || page.Continue == continuation {
			return ErrInvalid
		}
		seen["page:"+page.Continue] = true
		continuation = page.Continue
	}
	if len(orphans) == 0 {
		return nil
	}
	if err := h.fence(ctx, j, g); err != nil {
		return err
	}
	return h.opts.Ledger.PruneEventCursors(ctx, g.Ledger, orphans)
}
