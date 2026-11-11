package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"k8s.io/apimachinery/pkg/api/meta"
)

// Hook topics root produces in v0.2 (D379). query.blocked (per-node query
// path, sampling and privacy), vip.master_changed (node-local, no replay),
// client.seen and client.left (client names and addresses need the hide and
// redaction rules) are deferred; the webhook provider advertises only these.
const (
	topicRecordChanged   = "record.changed"
	topicZoneChanged     = "zone.changed"
	topicBlockingToggled = "blocking.toggled"
	topicIntegrationErr  = "integration.error"
	topicLeaderChanged   = "cluster.leader_changed"
)

// Cursor positions of root's event production. The kind names hold the
// store change-feed cursor of that kind; the others hold level digests.
const (
	positionTerm     = "term"
	positionBlocking = "blocking"
	positionResync   = "resync"
)

// integrationEventKinds are the watched kinds, in production order.
// ZoneState is the private committed head of a zone: on the SQLite runtime
// a record change commits a new serial there, not in the DNSZone object.
var integrationEventKinds = []store.Kind{"DNSZone", zk.KindZoneState, "DNSRecordSet", "Integration", "Kudurru"}

// errEventFeedReset is a cursor the change feed can no longer resume: it
// was compacted past it, or the store's watch lineage changed (a replica
// replacement or promotion).
var errEventFeedReset = errors.New("integration events: change feed cursor cannot resume")

// produceEvents is the host's Producer (D379). It only reads: the store's
// change feed after the cursor, the current term and Kudurru blocking state.
// Each event names its subject UID and a version that increases for that
// subject, so the delivery ID dedups a reproduction. Status writes that do
// not change what a topic reports reproduce an ID that is already queued.
func (r *integrationRuntime) produceEvents(ctx context.Context, g ih.ExecutionGuard, positions map[string]string, topics []string, limit int) (ih.EventBatch, error) {
	next := map[string]string{}
	for k, v := range positions {
		next[k] = v
	}
	term := integrationTermID(g)
	if positions == nil {
		// Baseline: the current positions, no events. A new hook instance
		// learns about changes from now on, never about every object.
		if err := r.eventBaseline(ctx, next); err != nil {
			return ih.EventBatch{}, err
		}
		next[positionTerm] = term
		return ih.EventBatch{Positions: next}, nil
	}
	wanted := func(topic string) bool { return slices.Contains(topics, topic) }
	var events []ih.ProducedEvent
	if next[positionTerm] != term {
		if wanted(topicLeaderChanged) {
			payload, _ := json.Marshal(map[string]any{"epoch": g.Ledger.Term.Epoch, "primaryID": g.Ledger.Term.PrimaryID})
			events = append(events, ih.ProducedEvent{Topic: topicLeaderChanged, Subject: "cluster/" + g.Ledger.Term.ClusterID, Version: term, Payload: payload})
		}
		next[positionTerm] = term
	}
	if _, resync := next[positionResync]; resync {
		return r.eventResync(ctx, next, events, wanted, limit)
	}
	for _, kind := range integrationEventKinds {
		if len(events) >= limit {
			break
		}
		feed, cursor, err := r.eventFeed(ctx, kind, next[string(kind)], limit-len(events))
		if errors.Is(err, errEventFeedReset) {
			// Never one event per object: one zone.changed per zone, then a
			// fresh baseline.
			next[positionResync] = ""
			return r.eventResync(ctx, next, events, wanted, limit)
		}
		if err != nil {
			return ih.EventBatch{}, err
		}
		next[string(kind)] = cursor
		for _, e := range feed {
			produced, ok, err := r.eventFor(e, next, wanted)
			if err != nil {
				return ih.EventBatch{}, err
			}
			if ok {
				events = append(events, produced)
			}
		}
	}
	return ih.EventBatch{Events: events, Positions: next}, nil
}

func integrationTermID(g ih.ExecutionGuard) string {
	t := g.Ledger.Term
	return kinds.Digest(fmt.Appendf(nil, "%s/%d/%s/%s", t.ClusterID, t.Epoch, t.PrimaryID, t.PromotionID))[:32]
}

// eventBaseline records the current feed cursor of every watched kind and
// the current blocking level.
func (r *integrationRuntime) eventBaseline(ctx context.Context, next map[string]string) error {
	for _, kind := range integrationEventKinds {
		_, cursor, err := r.eventFeed(ctx, kind, "", 0)
		if err != nil {
			return err
		}
		next[string(kind)] = cursor
	}
	var k public.Kudurru
	err := r.s.opts.Store.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &k)
	switch {
	case err == nil:
		next[positionBlocking] = blockingDigest(k.Status.Blocking)
	case errors.Is(err, store.ErrNotFound):
		next[positionBlocking] = blockingDigest(public.BlockingStatus{})
	default:
		return err
	}
	return nil
}

// eventCursorRefresh is how far the store head may run ahead of a kind's
// position before a tick with no change of that kind moves the position
// anyway. Below it the position stays, so an idle tick writes no cursor:
// the cursor's own write advances the head, and following the head would
// rewrite the cursor on every tick (D379). It is far below the change
// feed's retention (100,000 rows), so a quiet kind is never compacted
// past, and it bounds the feed rows a tick re-reads.
const eventCursorRefresh = 1024

// eventFeed reads the change feed of one kind after cursor until the
// watch's bookmark, keeping at most limit data events. It returns the
// cursor to resume from: the last kept event's, the bookmark's when
// nothing was cut, or cursor itself when the kind did not change and the
// head is less than eventCursorRefresh ahead. An empty cursor reads only
// the current position.
func (r *integrationRuntime) eventFeed(ctx context.Context, kind store.Kind, cursor string, limit int) ([]store.Event, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w, err := r.s.opts.Store.Watch(ctx, kind, store.WatchOptions{ResourceVersion: cursor})
	if errors.Is(err, sqlite.ErrWatchLineage) || errors.Is(err, sqlite.ErrResourceVersionTooNew) {
		return nil, "", errEventFeedReset
	}
	if err != nil {
		return nil, "", err
	}
	defer w.Stop()
	from, _ := cursorCounter(cursor)
	var out []store.Event
	cut := ""
	for {
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case e, ok := <-w.ResultChan():
			if !ok {
				return nil, "", ih.ErrUnavailable
			}
			if e.Type == store.Bookmark {
				if cursor != "" && r.s.opts.Store.FeedCompacted() > from {
					// The feed no longer holds every change after the
					// cursor, so what was read may be a relist. The watch
					// relists silently, and a kind with no objects relists
					// as a bare bookmark: ask the store, never infer.
					return nil, "", errEventFeedReset
				}
				if cut != "" {
					return out, cut, nil
				}
				if head, ok := cursorCounter(e.Cursor); cursor != "" && len(out) == 0 && ok && head >= from && head-from < eventCursorRefresh {
					return nil, cursor, nil
				}
				return out, e.Cursor, nil
			}
			if cursor == "" {
				// A baseline relists every object; only its bookmark counts.
				continue
			}
			if counter, ok := cursorCounter(e.Cursor); !ok || counter <= from {
				// A resumed watch only replays changes after the cursor: an
				// older position is the relist after a compaction.
				return nil, "", errEventFeedReset
			}
			if cut != "" {
				continue
			}
			out = append(out, e)
			if len(out) == limit {
				cut = e.Cursor
			}
		}
	}
}

func cursorCounter(cursor string) (uint64, bool) {
	if _, counter, ok := strings.Cut(cursor, ":"); ok {
		cursor = counter
	}
	n, err := strconv.ParseUint(cursor, 10, 64)
	return n, err == nil
}

// eventFor maps one change to at most one event. Payload keys are lowercase
// and carry identities and versions, never record data or credentials.
func (r *integrationRuntime) eventFor(e store.Event, next map[string]string, wanted func(string) bool) (ih.ProducedEvent, bool, error) {
	deleted := e.Type == store.Deleted
	switch o := e.Object.(type) {
	case *public.DNSRecordSet:
		if !wanted(topicRecordChanged) || o.UID == "" {
			return ih.ProducedEvent{}, false, nil
		}
		version := strconv.FormatInt(o.Generation, 10)
		if deleted {
			version = "deleted"
		}
		payload, err := json.Marshal(map[string]any{"change": eventChange(e.Type), "namespace": o.Namespace, "name": o.Name, "uid": string(o.UID), "zone": o.Spec.ZoneRef.Name, "generation": o.Generation})
		return ih.ProducedEvent{Topic: topicRecordChanged, Subject: string(o.UID), Version: version, Payload: payload}, true, err
	case *public.DNSZone:
		if !wanted(topicZoneChanged) || o.UID == "" {
			return ih.ProducedEvent{}, false, nil
		}
		return zoneEvent(o, eventChange(e.Type))
	case *zk.ZoneState:
		// A committed serial of a zone. Deleting or tombstoning the head
		// is the DNSZone's deletion, reported from the DNSZone itself; a
		// notification acknowledgment keeps the serial, so its version.
		if !wanted(topicZoneChanged) || deleted || o.Spec.Lifecycle != zk.LifecycleActive || o.Spec.Zone.UID == "" || o.Spec.Serial == 0 {
			return ih.ProducedEvent{}, false, nil
		}
		payload, err := json.Marshal(map[string]any{"change": "modified", "name": o.Spec.Zone.ObjectName, "uid": o.Spec.Zone.UID, "zone": o.Spec.Zone.Apex, "serial": o.Spec.Serial, "contentHash": o.Spec.ContentHash})
		return ih.ProducedEvent{Topic: topicZoneChanged, Subject: o.Spec.Zone.UID, Version: "s" + strconv.FormatUint(uint64(o.Spec.Serial), 10), Payload: payload}, true, err
	case *public.Integration:
		if !wanted(topicIntegrationErr) || deleted || o.UID == "" {
			return ih.ProducedEvent{}, false, nil
		}
		ready := meta.FindStatusCondition(o.Status.Conditions, "Ready")
		if ready == nil || ready.Status != "False" {
			return ih.ProducedEvent{}, false, nil
		}
		version := fmt.Sprintf("%s/%d/%d", ready.Reason, ready.LastTransitionTime.UnixNano(), ready.ObservedGeneration)
		payload, err := json.Marshal(map[string]any{"namespace": o.Namespace, "name": o.Name, "uid": string(o.UID), "type": o.Spec.Type, "reason": ready.Reason})
		return ih.ProducedEvent{Topic: topicIntegrationErr, Subject: string(o.UID), Version: version, Payload: payload}, true, err
	case *public.Kudurru:
		if deleted {
			return ih.ProducedEvent{}, false, nil
		}
		level := blockingDigest(o.Status.Blocking)
		if next[positionBlocking] == level {
			return ih.ProducedEvent{}, false, nil
		}
		next[positionBlocking] = level
		if !wanted(topicBlockingToggled) || o.UID == "" {
			return ih.ProducedEvent{}, false, nil
		}
		body := map[string]any{"enabled": !o.Status.Blocking.Disabled}
		if until := o.Status.Blocking.DisabledUntil; until != nil {
			body["disabledUntil"] = until.UTC()
		}
		payload, err := json.Marshal(body)
		return ih.ProducedEvent{Topic: topicBlockingToggled, Subject: string(o.UID), Version: o.ResourceVersion, Payload: payload}, true, err
	}
	return ih.ProducedEvent{}, false, nil
}

func eventChange(t store.EventType) string {
	switch t {
	case store.Added:
		return "added"
	case store.Deleted:
		return "deleted"
	}
	return "modified"
}

// zoneEvent versions a zone by its committed serial, so the status writes
// of one serial are one event and a resync of an unchanged zone dedups.
func zoneEvent(z *public.DNSZone, change string) (ih.ProducedEvent, bool, error) {
	version := "g" + strconv.FormatInt(z.Generation, 10)
	if z.Status.Serial != 0 {
		version = "s" + strconv.FormatUint(uint64(z.Status.Serial), 10)
	}
	if change == "deleted" {
		version = "deleted"
	}
	payload, err := json.Marshal(map[string]any{"change": change, "name": z.Name, "uid": string(z.UID), "zone": z.Spec.ZoneName, "serial": z.Status.Serial, "contentHash": z.Status.ContentHash})
	return ih.ProducedEvent{Topic: topicZoneChanged, Subject: string(z.UID), Version: version, Payload: payload}, true, err
}

func blockingDigest(b public.BlockingStatus) string {
	raw, _ := json.Marshal(b)
	return kinds.Digest(raw)[:32]
}

// eventResync replaces a cursor the feed cannot resume: one zone.changed per
// current zone, in name order, at most limit per tick (the resync position
// is the last zone emitted), then a fresh baseline of every kind.
func (r *integrationRuntime) eventResync(ctx context.Context, next map[string]string, events []ih.ProducedEvent, wanted func(string) bool, limit int) (ih.EventBatch, error) {
	after := next[positionResync]
	if wanted(topicZoneChanged) {
		page, err := r.s.opts.Store.List(ctx, "DNSZone", store.ListOptions{})
		if err != nil {
			return ih.EventBatch{}, err
		}
		zones := make([]*public.DNSZone, 0, len(page.Items))
		for _, item := range page.Items {
			if z, ok := item.(*public.DNSZone); ok && z.UID != "" && z.Name > after {
				zones = append(zones, z)
			}
		}
		slices.SortFunc(zones, func(a, b *public.DNSZone) int { return strings.Compare(a.Name, b.Name) })
		for _, z := range zones {
			if len(events) >= limit {
				next[positionResync] = after
				return ih.EventBatch{Events: events, Positions: next}, nil
			}
			e, _, err := zoneEvent(z, "resync")
			if err != nil {
				return ih.EventBatch{}, err
			}
			events = append(events, e)
			after = z.Name
		}
	}
	delete(next, positionResync)
	term := next[positionTerm]
	if err := r.eventBaseline(ctx, next); err != nil {
		return ih.EventBatch{}, err
	}
	next[positionTerm] = term
	return ih.EventBatch{Events: events, Positions: next}, nil
}
