package dnsdist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/edgeblock"
)

// Operator edge blocks (D388). Backends publish the canonical set on
// GET /internal/edge/blocks; the supervisor pulls it from every backend,
// keeps the newest revision of each owner, persists the result next to the
// rendered configuration and applies it to the running child through the
// console with dnsdist's own dynamic-block table. dnsdist enforces each
// block's remaining lifetime itself, so a block expires on the edge even if
// the supervisor or every backend is unreachable. A new child starts with
// an empty table; the supervisor reapplies the persisted set to it, which
// is how blocks survive dnsdist and supervisor restarts until they expire.

// EdgeBlockLister reads one backend's edge blocks
// (internalapi.Client.EdgeBlocks).
type EdgeBlockLister interface {
	EdgeBlocks(ctx context.Context, addr string) (internalapi.EdgeBlockList, error)
}

const (
	edgeBlocksFile        = "edge-blocks.json"
	edgeBlocksFileVersion = 1
	// The table dnsdist keeps carries this reason, so operators can tell
	// kudurru's blocks from rate-triggered ones in showDynBlocks().
	edgeBlockReason = "kudurru edge block "
	// One console statement adds at most this many blocks, keeping each
	// statement well under maxConsoleCommand.
	edgeBlocksPerStatement = 64
	edgeBlockApplyTick     = 250 * time.Millisecond
)

type edgeBlocksDocument struct {
	Version int                     `json:"version"`
	SavedAt time.Time               `json:"savedAt"`
	Blocks  []internalapi.EdgeBlock `json:"blocks"`
}

// edgeBlockState is owned by mu.
type edgeBlockState struct {
	desired        []internalapi.EdgeBlock // active, sorted by ID
	source         string                  // "backends", "persisted" or "" before the first answer
	installedChild *edge.Child
	installed      map[string]time.Time // ID → expiry installed in installedChild
	unknown        bool                 // installedChild's table is unknown after an unacknowledged update
	err            string
	pulled         time.Time
}

// combineEdgeBlocks keeps the highest revision of every owner (a lagging
// replica cannot resurrect a revoked block) and unions owners (independent
// static twins each publish their own set). Equal prefixes keep the later
// expiry. Expired entries are dropped.
func combineEdgeBlocks(lists []internalapi.EdgeBlockList, now time.Time) []internalapi.EdgeBlock {
	newest := map[string]internalapi.EdgeBlockList{}
	for _, l := range lists {
		if cur, ok := newest[l.Owner]; !ok || l.Revision > cur.Revision {
			newest[l.Owner] = l
		}
	}
	byPrefix := map[string]internalapi.EdgeBlock{}
	for _, owner := range slices.Sorted(maps.Keys(newest)) {
		for _, b := range newest[owner].Blocks {
			if !b.Until.After(now) {
				continue
			}
			if cur, ok := byPrefix[b.Prefix]; !ok || b.Until.After(cur.Until) {
				byPrefix[b.Prefix] = b
			}
		}
	}
	out := slices.Collect(maps.Values(byPrefix))
	slices.SortFunc(out, func(a, b internalapi.EdgeBlock) int { return strings.Compare(a.ID, b.ID) })
	if len(out) > edgeblock.MaxBlocks {
		out = out[:edgeblock.MaxBlocks]
	}
	return out
}

func activeEdgeBlocks(in []internalapi.EdgeBlock, now time.Time) []internalapi.EdgeBlock {
	return slices.DeleteFunc(slices.Clone(in), func(b internalapi.EdgeBlock) bool { return !b.Until.After(now) })
}

func (s *Supervisor) edgeBlocksPath() string { return filepath.Join(s.cfg.ConfigDir, edgeBlocksFile) }

// loadEdgeBlocks reads the persisted set; a missing, malformed or foreign
// file is an empty set (it is a restart cache, never canonical state).
func (s *Supervisor) loadEdgeBlocks(now time.Time) []internalapi.EdgeBlock {
	data, err := os.ReadFile(s.edgeBlocksPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("cannot read persisted edge blocks", "err", err)
		}
		return nil
	}
	var doc edgeBlocksDocument
	if len(data) > 2*edgeblock.MaxDocumentBytes || json.Unmarshal(data, &doc) != nil || doc.Version != edgeBlocksFileVersion ||
		(internalapi.EdgeBlockList{Owner: "persisted", Blocks: doc.Blocks}).Validate() != nil {
		s.log.Warn("ignoring malformed persisted edge blocks", "file", s.edgeBlocksPath())
		return nil
	}
	return activeEdgeBlocks(doc.Blocks, now)
}

func (s *Supervisor) saveEdgeBlocks(blocks []internalapi.EdgeBlock, now time.Time) error {
	data, err := json.Marshal(edgeBlocksDocument{Version: edgeBlocksFileVersion, SavedAt: now.UTC(), Blocks: blocks})
	if err != nil {
		return err
	}
	return edge.WriteFileAtomic(s.edgeBlocksPath(), data, 0o600)
}

// pullEdgeBlocks reads every backend once. ok is false when no backend
// answered with a set, in which case the previous set stays in force.
func (s *Supervisor) pullEdgeBlocks(ctx context.Context, eps []cluster.Endpoint, now time.Time) ([]internalapi.EdgeBlock, bool) {
	if len(eps) > maxResolverSources {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var mu sync.Mutex
	var lists []internalapi.EdgeBlockList
	jobs := make(chan cluster.Endpoint, len(eps))
	for _, e := range eps {
		if e.InternalAddress != "" {
			jobs <- e
		}
	}
	close(jobs)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for e := range jobs {
				if ctx.Err() != nil {
					return
				}
				l, err := s.cfg.Blocks.EdgeBlocks(ctx, e.InternalAddress)
				if err != nil {
					if !errors.Is(err, internalapi.ErrEdgeBlocksUnsupported) {
						s.log.Debug("edge block poll failed", "backend", e.InternalAddress, "err", err)
					}
					continue
				}
				mu.Lock()
				lists = append(lists, l)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(lists) == 0 {
		return nil, false
	}
	return combineEdgeBlocks(lists, now), true
}

func edgeBlockSeconds(until, now time.Time) int64 {
	return int64(math.Ceil(until.Sub(now).Seconds()))
}

// edgeBlockStatements renders the console statements that install blocks.
// clear wipes dnsdist's table first (there is no per-entry removal), which
// also drops blocks the rate rules added; those return if the client keeps
// exceeding the rate. Every value comes from a validated prefix, ID or
// integer, so no input reaches Lua unquoted.
func edgeBlockStatements(blocks []internalapi.EdgeBlock, clear bool, now time.Time) ([]string, error) {
	var out []string
	var current []string
	if clear {
		current = append(current, "clearDynBlocks()")
	}
	for _, b := range blocks {
		p, err := edgeblock.ParsePrefix(b.Prefix)
		if err != nil || !edgeblock.ValidID(b.ID) {
			return nil, edgeblock.ErrState
		}
		secs := edgeBlockSeconds(b.Until, now)
		if secs <= 0 {
			continue
		}
		current = append(current, fmt.Sprintf("addDynamicBlock(%s, %s, DNSAction.Drop, %d, %d)", luaString(p.Addr().String()), luaString(edgeBlockReason+b.ID), secs, p.Bits()))
		if len(current) >= edgeBlocksPerStatement {
			out = append(out, strings.Join(current, "; "))
			current = nil
		}
	}
	if len(current) > 0 {
		out = append(out, strings.Join(current, "; "))
	}
	return out, nil
}

// planEdgeBlocks decides what the running child needs. A new child gets the
// whole set; after an unacknowledged statement the table is unknown and is
// cleared and reinstalled; a removed block or a changed expiry clears and reinstalls (dnsdist
// only ever extends an entry); otherwise only new blocks are added.
func planEdgeBlocks(desired []internalapi.EdgeBlock, installed map[string]time.Time, fresh, unknown bool, now time.Time) (adds []internalapi.EdgeBlock, clear, noop bool) {
	if fresh {
		return desired, false, len(desired) == 0
	}
	if unknown {
		return desired, true, false
	}
	want := map[string]time.Time{}
	for _, b := range desired {
		want[b.ID] = b.Until
	}
	for id, until := range installed {
		if !until.After(now) {
			continue // dnsdist expired it on its own
		}
		if w, ok := want[id]; !ok || !w.Equal(until) {
			return desired, true, false
		}
	}
	for _, b := range desired {
		if until, ok := installed[b.ID]; !ok || !until.Equal(b.Until) {
			adds = append(adds, b)
		}
	}
	return adds, false, len(adds) == 0
}

// applyEdgeBlocks brings the running child to the desired set once. Console
// work runs outside mu; an acknowledgment counts only for the child it was
// sent to. An unacknowledged statement is reported and the whole set is
// resent (clear first) on the next tick; the child is not retired, because
// a failed abuse block must not take the edge out of service.
func (s *Supervisor) applyEdgeBlocks(ctx context.Context) {
	now := time.Now()
	s.mu.Lock()
	child := s.child
	st := &s.blocks
	st.desired = activeEdgeBlocks(st.desired, now)
	desired := slices.Clone(st.desired)
	installed := maps.Clone(st.installed)
	fresh := st.installedChild != child
	unknown := !fresh && st.unknown
	// A fresh status of this child shows its API (and console) is up; an
	// earlier statement would only fail and force a clear on the next tick.
	running := child != nil && child.Running() && !s.restarting && s.status != nil
	s.mu.Unlock()
	if !running {
		return
	}
	adds, clear, noop := planEdgeBlocks(desired, installed, fresh, unknown, now)
	if noop {
		s.mu.Lock()
		if s.child == child {
			st.installedChild, st.unknown = child, false
			st.installed = edgeBlockSet(desired)
			s.blocksGauge.Set(float64(len(desired)))
		}
		s.mu.Unlock()
		return
	}
	statements, err := edgeBlockStatements(adds, clear, now)
	for _, statement := range statements {
		if err != nil {
			break
		}
		err = s.console(ctx, statement)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child != child || ctx.Err() != nil {
		return
	}
	if err != nil {
		st.installedChild, st.installed, st.unknown = child, nil, true
		st.err = "edge block update was not acknowledged; resending the whole set"
		s.blockApplies.WithLabelValues("failed").Inc()
		return
	}
	next := map[string]time.Time{}
	if !clear {
		maps.Copy(next, installed)
	}
	for _, b := range adds {
		next[b.ID] = b.Until
	}
	st.installedChild, st.installed, st.unknown, st.err = child, next, false, ""
	s.blocksGauge.Set(float64(len(desired)))
	s.blockApplies.WithLabelValues("ok").Inc()
	s.log.Info("applied edge blocks", "active", len(desired), "added", len(adds), "cleared", clear)
}

func edgeBlockSet(blocks []internalapi.EdgeBlock) map[string]time.Time {
	out := make(map[string]time.Time, len(blocks))
	for _, b := range blocks {
		out[b.ID] = b.Until
	}
	return out
}

// runEdgeBlocks pulls every BlockPoll and applies every tick until ctx ends.
func (s *Supervisor) runEdgeBlocks(ctx context.Context) {
	pull := time.NewTimer(0)
	defer pull.Stop()
	apply := time.NewTicker(edgeBlockApplyTick)
	defer apply.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-pull.C:
			now := time.Now()
			s.mu.Lock()
			eps := slices.Clone(s.endpoints)
			s.mu.Unlock()
			if blocks, ok := s.pullEdgeBlocks(ctx, eps, now); ok {
				s.mu.Lock()
				changed := !slices.Equal(blocks, activeEdgeBlocks(s.blocks.desired, now))
				s.blocks.desired, s.blocks.source, s.blocks.pulled = blocks, "backends", now
				s.mu.Unlock()
				if changed {
					if err := s.saveEdgeBlocks(blocks, now); err != nil {
						s.log.Warn("cannot persist edge blocks; a restart before the next pull starts without them", "err", err)
					}
				}
			}
			pull.Reset(s.cfg.BlockPoll)
		case <-apply.C:
		}
		s.applyEdgeBlocks(ctx)
	}
}

// edgeBlockStatus is the /status projection: counts only, no prefixes.
func (s *Supervisor) edgeBlockStatusLocked() map[string]any {
	st := s.blocks
	out := map[string]any{
		"configured": s.cfg.Blocks != nil,
		"active":     len(st.desired),
		"installed":  st.installedChild != nil && st.installedChild == s.child && !st.unknown,
		"source":     st.source,
	}
	if st.err != "" {
		out["error"] = st.err
	}
	if !st.pulled.IsZero() {
		out["pulledAt"] = st.pulled.UTC()
	}
	return out
}
