package audit

import (
	"context"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// SQLiteStore is the part of *sqlite.Store the SQLite recorder uses.
type SQLiteStore interface {
	RecordAudit(ctx context.Context, e sqlite.AuditEntry) (sqlite.AuditEntry, error)
	ReadAudit(ctx context.Context, q sqlite.AuditQuery) ([]sqlite.AuditEntry, error)
}

var _ SQLiteStore = (*sqlite.Store)(nil)

// SQLite records events in the sqlite store's hash-chained audit table
// (state.db, §11.4) and reads that table back, including the rows the
// driver writes for every store mutation.
type SQLite struct {
	Store SQLiteStore
}

// Record implements Recorder.
func (s SQLite) Record(ctx context.Context, ev Event) error {
	manager := ev.Manager
	if manager == "" {
		manager = store.ManagerAPI
	}
	_, err := s.Store.RecordAudit(ctx, sqlite.AuditEntry{
		Time:    ev.Time,
		Actor:   ev.Actor.String(),
		Manager: manager,
		Action:  ev.Action,
		Key:     store.Key{Kind: store.Kind(ev.Resource.Kind), Namespace: ev.Resource.Namespace, Name: ev.Resource.Name},
		Result:  ev.Result,
		Reason:  reasonWithContext(ev),
		Before:  ev.Before,
		After:   ev.After,
	})
	return err
}

// reasonWithContext folds the §11.4 fields the audit table has no column
// for yet (authz path, request id, client address) into the reason.
func reasonWithContext(ev Event) string {
	var parts []string
	if ev.Reason != "" {
		parts = append(parts, ev.Reason)
	}
	var ctx []string
	if ev.Authz != "" {
		ctx = append(ctx, "authz="+ev.Authz)
	}
	if ev.Request.ID != "" {
		ctx = append(ctx, "request="+ev.Request.ID)
	}
	if ev.Request.IP != "" {
		ctx = append(ctx, "ip="+ev.Request.IP)
	}
	if len(ctx) > 0 {
		parts = append(parts, "("+strings.Join(ctx, " ")+")")
	}
	return strings.Join(parts, " ")
}

// Read implements Reader. The table has no actor index, so an actor filter
// pages through the table until the page is full.
func (s SQLite) Read(ctx context.Context, q Query) ([]Event, error) {
	limit := clampLimit(q.Limit)
	after := q.AfterID
	out := []Event{}
	for {
		rows, err := s.Store.ReadAudit(ctx, sqlite.AuditQuery{
			Kind: store.Kind(q.Kind), Name: q.Name, AfterID: after, Since: q.Since, Limit: limit,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			after = r.ID
			ev := fromEntry(r)
			if q.Actor != "" && ev.Actor.String() != q.Actor && ev.Actor.Name != q.Actor {
				continue
			}
			out = append(out, ev)
			if len(out) == limit {
				return out, nil
			}
		}
		if len(rows) < limit || q.Actor == "" {
			return out, nil
		}
	}
}

func fromEntry(r sqlite.AuditEntry) Event {
	actor := Actor{Name: r.Actor}
	if kind, name, ok := strings.Cut(r.Actor, ":"); ok && kind != "file" && kind != "integration" {
		actor = Actor{Kind: kind, Name: name}
	}
	return Event{
		ID: r.ID, Time: r.Time, Actor: actor, Action: r.Action,
		Resource: Resource{Kind: string(r.Key.Kind), Namespace: r.Key.Namespace, Name: r.Key.Name},
		Result:   r.Result, Reason: r.Reason, Before: r.Before, After: r.After,
		Manager: r.Manager, Hash: r.Hash, PrevHash: r.PrevHash,
	}
}
