// Package auditstore is the persisted audit trail (license feature "audit",
// docs/architecture/licensing.md): the recorder and reader over the sqlite
// store's hash-chained audit table. The event types and the always-on log
// recorder stay in internal/audit.
package auditstore

import (
	"context"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Verify checks the SQLite driver's original chain before conversion to events.
func (s SQLite) Verify(ctx context.Context) error { return s.Store.VerifyAudit(ctx) }

// SQLiteStore is the part of *sqlite.Store the SQLite recorder uses.
type SQLiteStore interface {
	RecordAudit(ctx context.Context, e sqlite.AuditEntry) (sqlite.AuditEntry, error)
	ReadAudit(ctx context.Context, q sqlite.AuditQuery) ([]sqlite.AuditEntry, error)
	VerifyAudit(ctx context.Context) error
}

var _ audit.Recorder = SQLite{}
var _ audit.Reader = SQLite{}
var _ SQLiteStore = (*sqlite.Store)(nil)

// SQLite records events in the sqlite store's hash-chained audit table
// (state.db, §11.4) and reads that table back, including the rows the
// driver writes for every store mutation.
type SQLite struct {
	Store SQLiteStore
}

// Record implements Recorder.
func (s SQLite) Record(ctx context.Context, ev audit.Event) error {
	manager := ev.Manager
	if manager == "" {
		manager = store.ManagerAPI
	}
	_, err := s.Store.RecordAudit(ctx, sqlite.AuditEntry{
		Time:            ev.Time,
		Actor:           ev.Actor.String(),
		Manager:         manager,
		Action:          ev.Action,
		Key:             store.Key{Kind: store.Kind(ev.Resource.Kind), Namespace: ev.Resource.Namespace, Name: ev.Resource.Name},
		Result:          ev.Result,
		Reason:          reasonWithContext(ev),
		Before:          ev.Before,
		After:           ev.After,
		ResourceVersion: ev.ResourceVersion, Authz: ev.Authz, RequestID: ev.Request.ID, ClientIP: ev.Request.IP, UserAgent: ev.Request.UA, RequestMethod: ev.Request.Method, RequestPath: ev.Request.Path,
	})
	return err
}

// reasonWithContext preserves the historical human-readable reason suffix.
// Dedicated authenticated columns carry attribution without parsing this text.
func reasonWithContext(ev audit.Event) string {
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
func (s SQLite) Read(ctx context.Context, q audit.Query) ([]audit.Event, error) {
	if q.Cursor != "" {
		return nil, audit.ErrBadCursor
	}
	limit := audit.ClampLimit(q.Limit)
	after, before := q.AfterID, q.BeforeID
	out := []audit.Event{}
	for {
		rows, err := s.Store.ReadAudit(ctx, sqlite.AuditQuery{
			Kind: store.Kind(q.Kind), Name: q.Name, AfterID: after, BeforeID: before, Since: q.Since, Limit: limit, Desc: q.Desc,
		})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if q.Desc {
				before = r.ID
			} else {
				after = r.ID
			}
			ev := audit.FromEntry(r)
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
