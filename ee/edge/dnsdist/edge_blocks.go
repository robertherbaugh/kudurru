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
//
// The newest revision is remembered per owner across pulls and restarts
// (edgeBlockOwner), so a backend serving an older revision than one already
// applied cannot bring back a revoked block or wipe a new one, also when the
// newer backend misses pulls. Only consecutive pulls reporting lower revisions
// for BlockRestoreHold count as a deliberate step back (a restored backup or
// a promoted follower that lagged).

// EdgeBlockLister reads one backend's edge blocks
// (internalapi.Client.EdgeBlocks).
type EdgeBlockLister interface {
	EdgeBlocks(ctx context.Context, addr string) (internalapi.EdgeBlockList, error)
}

const (
	edgeBlocksFile        = "edge-blocks.json"
	edgeBlocksFileVersion = 3
	// The file is local and written by the supervisor; this only bounds a
	// corrupted one before decoding.
	maxEdgeBlocksFileBytes = 8 << 20
	// Keep at most this many absent owners, preferring active blocks and then
	// newer floors.
	maxEdgeBlockOwners = 8
	// An owner's memory is useless once every block a lower revision can
	// carry has expired: those blocks were created before the remembered
	// revision was first seen and last at most MaxDuration (plus clock skew).
	edgeBlockOwnerMemory = edgeblock.MaxDuration + time.Hour
	// The table dnsdist keeps carries this reason, so operators can tell
	// kudurru's blocks from rate-triggered ones in showDynBlocks().
	edgeBlockReason = "kudurru edge block "
	// One console statement adds at most this many blocks, keeping each
	// statement well under maxConsoleCommand.
	edgeBlocksPerStatement = 64
	edgeBlockApplyTick     = 250 * time.Millisecond
)

type edgeBlocksDocument struct {
	Version int                       `json:"version"`
	SavedAt time.Time                 `json:"savedAt"`
	Blocks  []internalapi.EdgeBlock   `json:"blocks"`
	Owners  map[string]edgeBlockOwner `json:"owners"`
}

// edgeBlockOwner is the newest list applied for one owner. AcceptedAt is
// when Revision was first applied; LowerSince is when pulls began to report
// only a lower revision (zero while none does).
type edgeBlockOwner struct {
	Revision      uint64                  `json:"revision,string"`
	AcceptedAt    time.Time               `json:"acceptedAt"`
	LowerSince    time.Time               `json:"lowerSince,omitzero"`
	LastLowerAt   time.Time               `json:"lastLowerAt,omitzero"`
	MissingSince  time.Time               `json:"missingSince,omitzero"`
	LastMissingAt time.Time               `json:"lastMissingAt,omitzero"`
	Blocks        []internalapi.EdgeBlock `json:"blocks"`
}

// edgeBlockState is owned by mu.
type edgeBlockState struct {
	desired        []internalapi.EdgeBlock // active, sorted by ID
	owners         map[string]edgeBlockOwner
	persistDirty   bool   // the file does not hold desired and owners yet
	source         string // "backends", "persisted" or "" before the first answer
	installedChild *edge.Child
	installed      map[string]time.Time // ID → expiry installed in installedChild
	unknown        bool                 // installedChild's table is unknown after an unacknowledged update
	err            string
	pulled         time.Time
}

// combineEdgeBlocks keeps the highest revision of every owner among lists
// (mergeEdgeBlocks supplies the floor across pulls) and unions owners
// (independent static twins each publish their own set). Equal prefixes keep the later
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

// mergeEdgeBlocks folds one pull into the remembered owners and returns the
// new memory and the desired set. A list below the remembered revision of
// its owner is ignored until consecutive pulls show only lower revisions for
// hold. Successful pulls that omit an owner retain its unexpired blocks until
// consecutive omissions span hold, then remember only its floor.
func mergeEdgeBlocks(owners map[string]edgeBlockOwner, lists []internalapi.EdgeBlockList, hold, maxGap time.Duration, now time.Time) (map[string]edgeBlockOwner, []internalapi.EdgeBlock) {
	newest := map[string]internalapi.EdgeBlockList{}
	for _, l := range lists {
		if cur, ok := newest[l.Owner]; !ok || l.Revision > cur.Revision {
			newest[l.Owner] = l
		}
	}
	next := make(map[string]edgeBlockOwner, len(owners)+len(newest))
	for name, o := range owners {
		o.Blocks = activeEdgeBlocks(o.Blocks, now)
		if _, reported := newest[name]; reported {
			o.MissingSince, o.LastMissingAt = time.Time{}, time.Time{}
		} else {
			o.LowerSince, o.LastLowerAt = time.Time{}, time.Time{}
			if len(o.Blocks) > 0 {
				o.MissingSince = edgeBlockStreakStart(o.MissingSince, o.LastMissingAt, now, maxGap)
				o.LastMissingAt = now
				if now.Sub(o.MissingSince) >= hold {
					o.Blocks = nil
				}
			}
			if len(o.Blocks) == 0 {
				o.MissingSince, o.LastMissingAt = time.Time{}, time.Time{}
			}
			if len(o.Blocks) == 0 && now.Sub(o.AcceptedAt) > edgeBlockOwnerMemory {
				continue
			}
		}
		next[name] = o
	}
	for name, l := range newest {
		o, known := next[name]
		switch {
		case !known || l.Revision > o.Revision:
			o = edgeBlockOwner{Revision: l.Revision, AcceptedAt: now, Blocks: activeEdgeBlocks(l.Blocks, now)}
		case l.Revision == o.Revision:
			o.LowerSince, o.LastLowerAt, o.Blocks = time.Time{}, time.Time{}, activeEdgeBlocks(l.Blocks, now)
		default:
			o.LowerSince = edgeBlockStreakStart(o.LowerSince, o.LastLowerAt, now, maxGap)
			o.LastLowerAt = now
			if now.Sub(o.LowerSince) >= hold {
				o = edgeBlockOwner{Revision: l.Revision, AcceptedAt: now, Blocks: activeEdgeBlocks(l.Blocks, now)}
			}
		}
		next[name] = o
	}
	if extra := len(next) - len(newest) - maxEdgeBlockOwners; extra > 0 {
		var absent []string
		for name := range next {
			if _, reported := newest[name]; !reported {
				absent = append(absent, name)
			}
		}
		slices.SortFunc(absent, func(a, b string) int {
			left, right := next[a], next[b]
			if len(left.Blocks) == 0 && len(right.Blocks) > 0 {
				return -1
			}
			if len(right.Blocks) == 0 && len(left.Blocks) > 0 {
				return 1
			}
			if order := left.AcceptedAt.Compare(right.AcceptedAt); order != 0 {
				return order
			}
			return strings.Compare(a, b)
		})
		for _, name := range absent[:extra] {
			delete(next, name)
		}
	}
	effective := make([]internalapi.EdgeBlockList, 0, len(next))
	for name, o := range next {
		effective = append(effective, internalapi.EdgeBlockList{Owner: name, Revision: o.Revision, Blocks: o.Blocks})
	}
	return next, combineEdgeBlocks(effective, now)
}

func edgeBlockStreakStart(since, last, now time.Time, maxGap time.Duration) time.Time {
	if since.IsZero() || last.Before(since) || now.Before(last) || now.Sub(last) > maxGap {
		return now
	}
	return since
}

func interruptEdgeBlockStreaks(owners map[string]edgeBlockOwner) map[string]edgeBlockOwner {
	next := maps.Clone(owners)
	for name, o := range next {
		o.LowerSince, o.LastLowerAt = time.Time{}, time.Time{}
		o.MissingSince, o.LastMissingAt = time.Time{}, time.Time{}
		next[name] = o
	}
	return next
}

func activeEdgeBlocks(in []internalapi.EdgeBlock, now time.Time) []internalapi.EdgeBlock {
	return slices.DeleteFunc(slices.Clone(in), func(b internalapi.EdgeBlock) bool { return !b.Until.After(now) })
}

func (s *Supervisor) edgeBlocksPath() string { return filepath.Join(s.cfg.ConfigDir, edgeBlocksFile) }

// loadEdgeBlocks reads the persisted set and owner memory; a missing,
// malformed or foreign file is an empty set (it is a restart cache, never
// canonical state).
func (s *Supervisor) loadEdgeBlocks(now time.Time) ([]internalapi.EdgeBlock, map[string]edgeBlockOwner) {
	data, err := os.ReadFile(s.edgeBlocksPath())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Warn("cannot read persisted edge blocks", "err", err)
		}
		return nil, nil
	}
	var doc edgeBlocksDocument
	valid := len(data) <= maxEdgeBlocksFileBytes && json.Unmarshal(data, &doc) == nil && (doc.Version == 2 || doc.Version == edgeBlocksFileVersion) &&
		(internalapi.EdgeBlockList{Owner: "persisted", Blocks: doc.Blocks}).Validate() == nil && len(doc.Owners) <= maxEdgeBlockOwners+maxResolverSources
	for name, o := range doc.Owners {
		valid = valid && (internalapi.EdgeBlockList{Owner: name, Revision: o.Revision, Blocks: o.Blocks}).Validate() == nil
	}
	if !valid {
		s.log.Warn("ignoring malformed persisted edge blocks", "file", s.edgeBlocksPath())
		return nil, nil
	}
	if doc.Version == 2 {
		doc.Owners = interruptEdgeBlockStreaks(doc.Owners)
	}
	return activeEdgeBlocks(doc.Blocks, now), doc.Owners
}

func (s *Supervisor) saveEdgeBlocks(blocks []internalapi.EdgeBlock, owners map[string]edgeBlockOwner, now time.Time) error {
	data, err := json.Marshal(edgeBlocksDocument{Version: edgeBlocksFileVersion, SavedAt: now.UTC(), Blocks: blocks, Owners: owners})
	if err != nil {
		return err
	}
	return edge.WriteFileAtomic(s.edgeBlocksPath(), data, 0o600)
}

// pullEdgeBlocks reads every backend once. ok is false when no backend
// answered with a set, in which case the previous set stays in force.
func (s *Supervisor) pullEdgeBlocks(ctx context.Context, eps []cluster.Endpoint) ([]internalapi.EdgeBlockList, bool) {
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
	return lists, len(lists) > 0
}

func edgeBlockSeconds(until, now time.Time) int64 {
	return int64(math.Ceil(until.Sub(now).Seconds()))
}

// edgeBlockStatements renders the console statements that install blocks.
// clear wipes dnsdist's table first (there is no per-entry removal), which
// also drops blocks the rate rules added; those return if the client keeps
// exceeding the rate. Every value comes from a validated prefix, ID or
// integer, so no input reaches Lua unquoted.
func edgeBlockStatements(blocks []internalapi.EdgeBlock, wipe bool, now time.Time) ([]string, error) {
	var out []string
	var current []string
	if wipe {
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
func planEdgeBlocks(desired []internalapi.EdgeBlock, installed map[string]time.Time, fresh, unknown bool, now time.Time) (adds []internalapi.EdgeBlock, wipe, noop bool) {
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
	now := s.cfg.BlockNow()
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
	adds, wipe, noop := planEdgeBlocks(desired, installed, fresh, unknown, now)
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
	statements, err := edgeBlockStatements(adds, wipe, now)
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
	if !wipe {
		maps.Copy(next, installed)
	}
	for _, b := range adds {
		next[b.ID] = b.Until
	}
	st.installedChild, st.installed, st.unknown, st.err = child, next, false, ""
	s.blocksGauge.Set(float64(len(desired)))
	s.blockApplies.WithLabelValues("ok").Inc()
	s.log.Info("applied edge blocks", "active", len(desired), "added", len(adds), "cleared", wipe)
}

// sameEdgeBlockOwners reports whether two owner memories persist the same.
func sameEdgeBlockOwners(a, b map[string]edgeBlockOwner) bool {
	return maps.EqualFunc(a, b, func(x, y edgeBlockOwner) bool {
		return x.Revision == y.Revision && x.AcceptedAt.Equal(y.AcceptedAt) &&
			x.LowerSince.Equal(y.LowerSince) && x.LastLowerAt.Equal(y.LastLowerAt) &&
			x.MissingSince.Equal(y.MissingSince) && x.LastMissingAt.Equal(y.LastMissingAt) && slices.Equal(x.Blocks, y.Blocks)
	})
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
	ticks := s.cfg.blockPollTicks
	var pull *time.Timer
	if ticks == nil {
		pull = time.NewTimer(0)
		defer pull.Stop()
		ticks = pull.C
	}
	apply := time.NewTicker(edgeBlockApplyTick)
	defer apply.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			s.syncEdgeBlocks(ctx)
			if pull != nil {
				pull.Reset(s.cfg.BlockPoll)
			}
		case <-apply.C:
		}
		s.applyEdgeBlocks(ctx)
	}
}

func (s *Supervisor) syncEdgeBlocks(ctx context.Context) {
	s.mu.Lock()
	eps := slices.Clone(s.endpoints)
	s.mu.Unlock()
	lists, ok := s.pullEdgeBlocks(ctx, eps)
	if ctx.Err() != nil {
		return
	}
	now := s.cfg.BlockNow().UTC()
	s.mu.Lock()
	st := &s.blocks
	active := activeEdgeBlocks(st.desired, now)
	var owners map[string]edgeBlockOwner
	blocks := active
	if ok {
		owners, blocks = mergeEdgeBlocks(st.owners, lists, s.cfg.BlockRestoreHold, 3*s.cfg.BlockPoll, now)
		st.source, st.pulled = "backends", now
	} else {
		owners = interruptEdgeBlockStreaks(st.owners)
	}
	if !slices.Equal(blocks, active) || !sameEdgeBlockOwners(owners, st.owners) {
		st.persistDirty = true
	}
	st.desired, st.owners = blocks, owners
	dirty := st.persistDirty
	s.mu.Unlock()
	if !dirty {
		return
	}
	if err := s.saveEdgeBlocks(blocks, owners, now); err != nil {
		s.log.Warn("cannot persist edge blocks; retrying on the next pull", "err", err)
		return
	}
	s.mu.Lock()
	st.persistDirty = false
	s.mu.Unlock()
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
