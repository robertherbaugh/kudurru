package host

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProducedEvent is one event root derived from its own durable state
// (D379). Subject is the stable identity of what changed (an object UID);
// Version increases for that subject. Together with the instance and the
// topic they are the delivery ID, so producing the same change again, after
// a crash between the queue and the cursor, finds the queued row instead of
// a second event.
type ProducedEvent struct {
	Topic   string
	Subject string
	Version string
	Payload json.RawMessage
}

// EventBatch is what root read since the cursor: at most the requested
// number of events, and the positions to resume from once they are queued.
type EventBatch struct {
	Events    []ProducedEvent
	Positions map[string]string
}

// Producer reads root's change feed after positions (nil before the first
// production: a baseline that returns no events) and maps it to sanitized,
// bounded events of the given topics. It must not write: every write of the
// production goes through the host's fenced ledger writer.
type Producer func(ctx context.Context, g ExecutionGuard, positions map[string]string, topics []string, limit int) (EventBatch, error)

// Production bounds (D379): the 2 s event tick is the debounce, a tick
// queues at most maxProducePerTick events, and production waits while the
// instance already has maxPendingDeliveries undelivered events or
// maxInstanceDeliveries rows in all. Retention evicts an instance's oldest
// terminal rows above maxTerminalDeliveries, so a peer that acknowledges
// promptly is bounded by count too, not only by the 25 h retention.
const (
	maxProducePerTick     = 32
	maxPendingDeliveries  = 256
	maxTerminalDeliveries = 128
	maxInstanceDeliveries = maxPendingDeliveries + maxTerminalDeliveries
)

// DeliveryID is the stable at-least-once identity of a produced event.
func DeliveryID(owner InstanceID, e ProducedEvent) string {
	raw, _ := json.Marshal(struct {
		UID, Topic, Subject, Version string
	}{owner.UID, e.Topic, e.Subject, e.Version})
	return kinds.Digest(raw)
}

// produceEvents queues what root produced since the cursor, then advances
// the cursor. A crash or refusal between the two leaves the cursor behind:
// the next tick produces the same events with the same IDs and skips them.
func (h *Host) produceEvents(ctx context.Context, j *job) error {
	if h.opts.Produce == nil {
		return nil
	}
	f, g, err := h.captured(j)
	if err != nil {
		return err
	}
	if ctx, err = h.scopedOriginal(ctx, j, f, g); err != nil {
		return err
	}
	hook, ok := j.provider.(DeliveryHook)
	if roles, err := providerRoles(j.provider); err != nil || !roles.Hook || !ok {
		return ErrUnsupported
	}
	topics := hook.Topics()
	if len(topics) == 0 || len(topics) > 128 {
		return nil
	}
	queued, err := h.deliveries(ctx, g.Ledger.Owner)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	pending := 0
	for _, d := range queued {
		existing[d.Spec.DeliveryID] = true
		if !d.Spec.Acknowledged && !d.Spec.DeadLetter {
			pending++
		}
	}
	if pending >= maxPendingDeliveries || len(queued) >= maxInstanceDeliveries {
		// The peer is behind, or retention has not yet evicted the oldest
		// terminal rows: keep the cursor where it is. If the feed is
		// compacted meanwhile, root falls back to one event per zone.
		return nil
	}
	cursor, err := h.opts.Ledger.ReadEventCursor(ctx, g.Ledger.Owner)
	if errors.Is(err, store.ErrNotFound) {
		cursor, err = nil, nil
	}
	if err != nil {
		return err
	}
	var positions map[string]string
	if cursor != nil {
		positions = maps.Clone(cursor.Spec.Positions)
	}
	limit := min(maxProducePerTick, maxPendingDeliveries-pending, maxInstanceDeliveries-len(queued))
	batch, err := h.opts.Produce(ctx, g, positions, slices.Clone(topics), limit)
	if err != nil {
		return err
	}
	if len(batch.Events) > limit || len(batch.Positions) > kinds.MaxCursorPositions {
		return ErrInvalid
	}
	now := h.opts.Now().UTC()
	for _, e := range batch.Events {
		if !slices.Contains(topics, e.Topic) || e.Subject == "" || e.Version == "" || len(e.Payload) > kinds.MaxPayloadBytes || !json.Valid(e.Payload) {
			return ErrInvalid
		}
		if containsSecret(e.Payload, f.instance.secrets) {
			return ErrInvalid
		}
		id := DeliveryID(g.Ledger.Owner, e)
		if existing[id] {
			continue
		}
		if err := h.fence(ctx, j, g); err != nil {
			return err
		}
		d := &kinds.IntegrationDelivery{ObjectMeta: metav1.ObjectMeta{Name: kinds.DeliveryName(g.Ledger.Owner, id)}, Spec: kinds.IntegrationDeliverySpec{FormatVersion: kinds.FormatVersion, Owner: g.Ledger.Owner, DeliveryID: id, Topic: e.Topic, Payload: slices.Clone(e.Payload), CreatedAt: now, Deadline: now.Add(24 * time.Hour), NextAttempt: now}}
		if _, err := h.opts.Ledger.QueueDelivery(ctx, g.Ledger, d); err != nil {
			return err
		}
		existing[id] = true
	}
	next := &kinds.IntegrationEventCursor{ObjectMeta: metav1.ObjectMeta{Name: kinds.EventCursorName(g.Ledger.Owner)}, Spec: kinds.IntegrationEventCursorSpec{FormatVersion: kinds.FormatVersion, Owner: g.Ledger.Owner, Positions: maps.Clone(batch.Positions), UpdatedAt: now}}
	if next.Spec.Positions == nil {
		next.Spec.Positions = map[string]string{}
	}
	if cursor != nil {
		if maps.Equal(cursor.Spec.Positions, next.Spec.Positions) {
			// Nothing new: no write, so an idle instance does not advance
			// the store revision every tick.
			return nil
		}
		next.ObjectMeta = cursor.ObjectMeta
	}
	if err := h.fence(ctx, j, g); err != nil {
		return err
	}
	_, err = h.opts.Ledger.SaveEventCursor(ctx, g.Ledger, next)
	return err
}
