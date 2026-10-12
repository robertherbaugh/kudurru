package audit

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// StateSource is the sqlite driver's audit table in state.db as a
// shipping Source: a cluster-wide store receives every node's rows while
// state.db keeps its own chain (D159).
type StateSource struct {
	store *sqlite.Store
	id    string
	node  string
}

var _ Source = (*StateSource)(nil)

// NewStateSource names state.db's audit rows by their file's source id.
func NewStateSource(ctx context.Context, st *sqlite.Store, node string) (*StateSource, error) {
	id, err := st.AuditSourceID(ctx)
	if err != nil {
		return nil, err
	}
	return &StateSource{store: st, id: id, node: node}, nil
}

// SourceID implements Source.
func (s *StateSource) SourceID() string { return s.id }

// Node implements Source.
func (s *StateSource) Node() string { return s.node }

// Kind names the source in the central database.
func (s *StateSource) Kind() string { return "state" }

// ReadAfter implements Source.
func (s *StateSource) ReadAfter(ctx context.Context, after int64, n int) ([]Event, error) {
	rows, err := s.store.ReadAudit(ctx, sqlite.AuditQuery{AfterID: after, Limit: n})
	if err != nil {
		return nil, err
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		ev := fromEntry(r)
		ev.Node = s.node
		out = append(out, ev)
	}
	return out, nil
}
