package zonecache

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
)

// startupFallback loads local.db when the store has not synced within
// StartupTimeout (§4.4).
func (c *Cache) startupFallback(ctx context.Context) {
	if !sleep(ctx, c.opts.StartupTimeout) {
		return
	}
	if c.Synced() {
		return
	}
	if c.opts.LocalDB == nil {
		c.setNoSnapshot("no local.db configured")
		c.log.Warn("store not synced within the startup timeout and no local.db is configured; not ready until it syncs", "timeout", c.opts.StartupTimeout)
		return
	}
	snap, err := c.opts.LocalDB.Load(ctx)
	switch {
	case errors.Is(err, localdb.ErrNoSnapshot):
		c.setNoSnapshot("local.db holds no snapshot yet")
		c.log.Warn("store not synced within the startup timeout and local.db is empty; not ready until it syncs", "timeout", c.opts.StartupTimeout)
		return
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		c.setNoSnapshot("local.db snapshot unreadable: " + err.Error())
		c.log.Error("store not synced within the startup timeout and local.db could not be read", "err", err)
		return
	}
	if !c.serveSnapshot(snap) {
		return
	}
	c.log.Warn("store not synced within the startup timeout; serving from the local.db snapshot",
		"timeout", c.opts.StartupTimeout, "objects", len(snap.Objects), "age", time.Since(snap.WrittenAt).Round(time.Second),
		"maxAge", c.opts.MaxAge, "resourceVersion", snap.HighWaterMark(), "skipped", len(snap.Skipped))
}

func (c *Cache) setNoSnapshot(why string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.syncedAll {
		c.noSnapshot = why
	}
}

// serveSnapshot switches to serving snap unless the store synced
// meanwhile. It reports whether it switched.
func (c *Cache) serveSnapshot(snap *localdb.Snapshot) bool {
	watched := make(map[store.Kind]bool, len(c.kinds))
	for _, k := range c.kinds {
		watched[k] = true
	}
	byKind := make(map[store.Kind][]store.Object, len(c.kinds))
	for _, k := range c.kinds {
		byKind[k] = nil
	}
	for _, o := range snap.Objects {
		if k := store.KindOf(o); watched[k] {
			byKind[k] = append(byKind[k], o)
		}
	}
	rvs := maps.Clone(snap.ResourceVersions)
	idx := c.indexOf(byKind, nil, rvs)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.syncedAll {
		return false
	}
	c.state = FromSnapshot
	c.snapIdx = idx
	c.snapWritten = snap.WrittenAt
	c.noSnapshot = ""
	c.conditions[ReasonServingFromSnapshot] = Condition{
		Reason: ReasonServingFromSnapshot,
		Message: fmt.Sprintf("store not synced within %s; serving %d objects from local.db written %s",
			c.opts.StartupTimeout, idx.total(), snap.WrittenAt.UTC().Format(time.RFC3339)),
		Since: time.Now(),
	}
	c.m.serving.Set(1)
	c.markInitialized()
	c.trigger()
	return true
}

// snapshotAgeSeconds backs kudurru_store_snapshot_age_seconds.
func (c *Cache) snapshotAgeSeconds() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != FromSnapshot {
		return 0
	}
	return time.Since(c.snapWritten).Seconds()
}

// persistLoop writes local.db after every sync (persistNow) and on each
// PersistInterval tick when the live set changed; on a clean tick older
// than RefreshInterval it only refreshes the snapshot's timestamp.
func (c *Cache) persistLoop(ctx context.Context) {
	tick := time.NewTicker(c.opts.PersistInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalPersistTimeout)
			if err := c.persistOnce(wctx); err != nil {
				c.log.Warn("final local.db snapshot write failed", "err", err)
			}
			cancel()
			return
		case <-c.persistNow:
		case <-tick.C:
		}
		if err := c.persistOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("local.db snapshot write failed; retrying on the next tick", "err", err)
		}
	}
}

// persistOnce writes the live set when it changed since the last write, or
// refreshes the stored timestamp when the last confirmation is older than
// RefreshInterval. It does nothing unless live.
func (c *Cache) persistOnce(ctx context.Context) error {
	c.mu.Lock()
	if c.state != Live {
		c.mu.Unlock()
		return nil
	}
	if !c.persistDirty {
		due := time.Since(c.lastConfirm) >= c.opts.RefreshInterval
		c.mu.Unlock()
		if !due {
			return nil
		}
		now := time.Now()
		err := c.opts.LocalDB.Touch(ctx, now)
		if errors.Is(err, localdb.ErrNoSnapshot) {
			c.mu.Lock()
			c.persistDirty = true
			c.mu.Unlock()
			return c.persistOnce(ctx)
		}
		if err != nil {
			return c.persistFailed(err)
		}
		c.persistSucceeded(now)
		return nil
	}
	n := 0
	for _, set := range c.objects {
		n += len(set)
	}
	objs := make([]store.Object, 0, n)
	for _, k := range c.kinds {
		for _, o := range c.objects[k] {
			objs = append(objs, o)
		}
	}
	rvs := maps.Clone(c.rvs)
	c.persistDirty = false
	c.mu.Unlock()

	now := time.Now()
	snap := &localdb.Snapshot{Objects: objs, ResourceVersions: rvs, ClusterID: c.st.Info().ClusterID, WrittenAt: now}
	if err := c.opts.LocalDB.Write(ctx, snap); err != nil {
		c.mu.Lock()
		c.persistDirty = true
		c.mu.Unlock()
		return c.persistFailed(err)
	}
	c.persistSucceeded(now)
	c.log.Debug("local.db snapshot written", "objects", len(objs), "took", time.Since(now))
	return nil
}

func (c *Cache) persistFailed(err error) error {
	c.m.snapshotWrites.WithLabelValues("error").Inc()
	c.mu.Lock()
	if _, ok := c.conditions[ReasonSnapshotWriteFailed]; !ok {
		c.conditions[ReasonSnapshotWriteFailed] = Condition{Reason: ReasonSnapshotWriteFailed, Message: err.Error(), Since: time.Now()}
	}
	c.mu.Unlock()
	return err
}

func (c *Cache) persistSucceeded(at time.Time) {
	c.m.snapshotWrites.WithLabelValues("ok").Inc()
	c.m.lastWrite.Set(float64(at.UnixNano()) / 1e9)
	c.mu.Lock()
	c.lastConfirm = at
	delete(c.conditions, ReasonSnapshotWriteFailed)
	c.mu.Unlock()
}

// total returns the number of indexed objects.
func (idx *index) total() int {
	n := 0
	for _, ki := range idx.byKind {
		n += len(ki.items)
	}
	return n
}
