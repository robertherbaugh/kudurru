package internalapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// PathEdgeBlocks is GET /internal/edge/blocks: the operator-initiated edge
// blocks (D388) this node currently serves, polled by edge supervisors over
// the same authenticated channel as /internal/zones. It carries prefixes,
// IDs and absolute expiries only, never the operator's reason or identity.
const PathEdgeBlocks = "/internal/edge/blocks"

const maxEdgeBlockBytes = 128 << 10

// ErrEdgeBlocksUnsupported is returned by Client.EdgeBlocks for a peer
// without the route (an older node, or a node without a block owner).
var ErrEdgeBlocksUnsupported = errors.New("internalapi: peer does not serve " + PathEdgeBlocks)

// EdgeBlock is one active block as an edge applies it.
type EdgeBlock struct {
	ID     string    `json:"id"`
	Prefix string    `json:"prefix"`
	Until  time.Time `json:"until"`
}

// EdgeBlockList is the body of GET /internal/edge/blocks. Owner names the
// canonical store the set belongs to (the cluster ID, or the Kubernetes
// runtime ConfigMap UID); Revision orders copies of the same owner, so an
// edge prefers the newest one over a lagging replica.
type EdgeBlockList struct {
	Node       string      `json:"node"`
	Owner      string      `json:"owner"`
	Revision   uint64      `json:"revision,string"`
	ObservedAt time.Time   `json:"observedAt"`
	Blocks     []EdgeBlock `json:"blocks"`
}

// EdgeBlockListFrom projects the active blocks of a stored state at now.
func EdgeBlockListFrom(node, owner string, s edgeblock.State, now time.Time) EdgeBlockList {
	out := EdgeBlockList{Node: node, Owner: owner, Revision: s.Revision, ObservedAt: now.UTC(), Blocks: []EdgeBlock{}}
	for _, b := range s.Active(now) {
		out.Blocks = append(out.Blocks, EdgeBlock{ID: b.ID, Prefix: b.Prefix, Until: b.Until})
	}
	return out
}

// Validate refuses a list an edge must not apply: an unnamed owner, too
// many entries, malformed IDs or prefixes, duplicates, or zero expiries.
func (l EdgeBlockList) Validate() error {
	if l.Owner == "" || len(l.Owner) > 256 || len(l.Node) > 256 || len(l.Blocks) > edgeblock.MaxBlocks {
		return edgeblock.ErrState
	}
	ids, prefixes := map[string]bool{}, map[string]bool{}
	for _, b := range l.Blocks {
		p, err := edgeblock.ParsePrefix(b.Prefix)
		if !edgeblock.ValidID(b.ID) || err != nil || p.String() != b.Prefix || b.Until.IsZero() || ids[b.ID] || prefixes[b.Prefix] {
			return edgeblock.ErrState
		}
		ids[b.ID], prefixes[b.Prefix] = true, true
	}
	return nil
}

func (s *Server) handleEdgeBlocks(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	list, err := s.opts.EdgeBlocks(r.Context())
	if err != nil || list == nil || r.Context().Err() != nil || list.Validate() != nil {
		http.Error(w, "edge blocks unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(list)
}

// EdgeBlocks fetches GET /internal/edge/blocks from addr (host:port). The
// list is validated before it is returned.
func (c *Client) EdgeBlocks(ctx context.Context, addr string) (EdgeBlockList, error) {
	resp, err := c.get(ctx, addr, PathEdgeBlocks)
	if errors.Is(err, ErrListNotFound) {
		return EdgeBlockList{}, ErrEdgeBlocksUnsupported
	}
	if err != nil {
		return EdgeBlockList{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEdgeBlockBytes+1))
	if err != nil {
		return EdgeBlockList{}, err
	}
	if len(data) > maxEdgeBlockBytes {
		return EdgeBlockList{}, ErrTooLarge
	}
	var list EdgeBlockList
	if err := json.Unmarshal(data, &list); err != nil {
		return EdgeBlockList{}, fmt.Errorf("internalapi: decode edge blocks from %s: %w", addr, err)
	}
	if err := list.Validate(); err != nil {
		return EdgeBlockList{}, fmt.Errorf("internalapi: edge blocks from %s: %w", addr, err)
	}
	return list, nil
}
