package sqlite

import (
	"context"
	"errors"
)

// RecordAudit appends an event in the original command transaction. An error
// poisons the command; rollback and final authorization failures discard it.
// This node-local record alone does not advance the replicated generation.
func (t *CommandTx) RecordAudit(ctx context.Context, e AuditEntry) error {
	if err := t.valid(ctx); err != nil {
		return err
	}
	if e.Action == "" {
		return t.poison(errors.New("store/sqlite: audit entry needs an Action"))
	}
	if e.Time.IsZero() {
		e.Time = t.s.opts.Now()
	}
	if e.Result == "" {
		e.Result = AuditSuccess
	}
	if e.Actor == "" {
		e.Actor = t.s.opts.ActorFromContext(ctx)
	}
	if err := insertAudit(ctx, t.q, &e); err != nil {
		return t.poison(err)
	}
	t.localChanged = true
	return nil
}
