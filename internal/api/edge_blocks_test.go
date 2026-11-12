package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// memEdgeBlocks is the model over an in-memory state, as a driver would
// apply it inside its command.
type memEdgeBlocks struct {
	mu    sync.Mutex
	state edgeblock.State
	now   time.Time
	last  EdgeBlockChange
}

func (m *memEdgeBlocks) ListEdgeBlocks(context.Context) (*EdgeBlockList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return EdgeBlockListFromState(m.state, m.now), nil
}

func (m *memEdgeBlocks) CreateEdgeBlock(_ context.Context, c EdgeBlockChange) (*EdgeBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = c
	id, _ := edgeblock.NewID()
	next, b, err := edgeblock.Add(m.state, edgeblock.Request{Prefix: c.Prefix, Duration: c.Duration, Until: c.Until, Reason: c.Reason, Actor: "user:operator"}, id, m.now)
	if err != nil {
		return nil, err
	}
	m.state = next
	out := EdgeBlockFromStored(b)
	return &out, nil
}

func (m *memEdgeBlocks) RevokeEdgeBlock(_ context.Context, id string) (*EdgeBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, b, err := edgeblock.Revoke(m.state, id, m.now)
	if err != nil {
		return nil, err
	}
	m.state = next
	out := EdgeBlockFromStored(b)
	return &out, nil
}

// TestEdgeBlockRoutes: permissions (viewer cannot read, auditor reads but
// cannot write, operator writes), request bounds, conflict and not-found
// mapping, the external audit of both commands, and 501 without a driver.
func TestEdgeBlockRoutes(t *testing.T) {
	store := &memEdgeBlocks{state: edgeblock.Empty(), now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	h := newHarness(t, func(o *Options) {
		o.EdgeBlocks = store
		o.AuditEdgeBlocks = true
	})
	base := BasePath + "/edge/blocks"
	h.do(http.MethodGet, base, "viewer", nil).want(http.StatusForbidden)
	h.do(http.MethodPost, base, "auditor", map[string]any{"prefix": "192.0.2.0/24", "seconds": 600}).want(http.StatusForbidden)
	for _, body := range []map[string]any{
		{"prefix": "192.0.2.0/24"},
		{"prefix": "192.0.2.0/24", "seconds": 30},
		{"prefix": "192.0.2.0/24", "seconds": 700000},
		{"prefix": "192.0.2.0/24", "seconds": 600, "until": "2026-11-10T13:00:00Z"},
		{"prefix": "10.0.0.0/8", "seconds": 600},
		{"prefix": "not-an-address", "seconds": 600},
	} {
		h.do(http.MethodPost, base, "operator", body).want(http.StatusBadRequest)
	}
	var created EdgeBlock
	h.do(http.MethodPost, base, "operator", map[string]any{"prefix": "192.0.2.9/24", "seconds": 600, "reason": "abuse"}).want(http.StatusCreated).json(&created)
	if created.Prefix != "192.0.2.0/24" || !created.Until.Equal(store.now.Add(10*time.Minute)) || created.Reason != "abuse" || !edgeblock.ValidID(created.ID) {
		t.Fatalf("created %+v", created)
	}
	if store.last.Duration != 10*time.Minute {
		t.Fatalf("change %+v", store.last)
	}
	h.do(http.MethodPost, base, "operator", map[string]any{"prefix": "192.0.2.0/24", "until": "2026-11-10T14:00:00Z"}).want(http.StatusConflict)
	var list EdgeBlockList
	h.do(http.MethodGet, base, "auditor", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID || list.Revision != "1" || list.MaxBlocks != edgeblock.MaxBlocks || list.MinSeconds != 60 || list.MinIPv4Bits != 16 {
		t.Fatalf("list %+v", list)
	}
	h.do(http.MethodDelete, base+"/0000000000000000", "operator", nil).want(http.StatusNotFound)
	h.do(http.MethodDelete, base+"/NOTHEX", "operator", nil).want(http.StatusBadRequest)
	h.do(http.MethodDelete, base+"/"+created.ID, "auditor", nil).want(http.StatusForbidden)
	var revoked EdgeBlock
	h.do(http.MethodDelete, base+"/"+created.ID, "operator", nil).want(http.StatusOK).json(&revoked)
	if revoked.ID != created.ID {
		t.Fatalf("revoked %+v", revoked)
	}
	h.do(http.MethodGet, base, "operator", nil).want(http.StatusOK).json(&list)
	if len(list.Items) != 0 || list.Revision != "2" {
		t.Fatalf("after revoke %+v", list)
	}
	events, err := h.audit.Read(t.Context(), audit.Query{})
	if err != nil {
		t.Fatal(err)
	}
	var creates, revokes, failed int
	for _, e := range events {
		switch {
		case e.Action == "edgeblock.create" && e.Result == audit.ResultSuccess && e.Resource.Name == created.ID && e.Actor.Name == "operator":
			creates++
		case e.Action == "edgeblock.revoke" && e.Result == audit.ResultSuccess && e.Resource.Name == created.ID:
			revokes++
		case e.Result == audit.ResultError && (e.Action == "edgeblock.create" || e.Action == "edgeblock.revoke"):
			failed++
		}
	}
	// Prefixes the model refuses, the duplicate and the unknown revoke are
	// audited as errors; requests refused before the driver (shape, bounds)
	// are not commands, and permission refusals are audited as denials.
	if creates != 1 || revokes != 1 || failed != 4 {
		t.Fatalf("audit creates=%d revokes=%d failed=%d: %+v", creates, revokes, failed, events)
	}

	none := newHarness(t, func(*Options) {})
	none.do(http.MethodGet, base, "operator", nil).want(http.StatusNotImplemented)
	none.do(http.MethodPost, base, "operator", map[string]any{"prefix": "192.0.2.0/24", "seconds": 600}).want(http.StatusNotImplemented)
}
