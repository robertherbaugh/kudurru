package audit

import (
	"strings"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// FromEntry converts a row of the sqlite audit table to an Event.
func FromEntry(r sqlite.AuditEntry) Event {
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
