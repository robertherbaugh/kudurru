package cache

import "github.com/robertherbaugh/kudurru/internal/pipeline"

// RebindRestored promotes only matching persisted semantic scopes into
// this process's publication epoch. Live old workers/entries are untouched.
// Call once for each captured scope before its first lookup.
func (c *Cache) RebindRestored(scope pipeline.CacheScope) {
	c.publication.Lock()
	defer c.publication.Unlock()
	var entries []*entry
	for i := range c.shards {
		sh := &c.shards[i]
		sh.mu.Lock()
		for _, e := range sh.m {
			if e.restored && e.metadata.Scope.ID == scope.ID {
				sh.remove(e)
				entries = append(entries, e)
			}
		}
		sh.mu.Unlock()
	}
	for _, e := range entries {
		c.m.entries(e.kind, -1)
		p, ok := e.key.Parts()
		if !ok {
			continue
		}
		k := MakeScopedKey(scope, p.Name, p.Type, p.Class, p.DO, p.CD, p.ECS)
		// The epoch changes the hash; reinsert into the destination shard.
		// Publication locking excludes live insertion, so a present entry
		// belongs to current work and must win over this restored copy.
		sh := c.shard(k)
		sh.mu.Lock()
		present := sh.m[k] != nil
		sh.mu.Unlock()
		if present {
			continue
		}
		e.key = k
		e.metadata.Scope = scope
		e.restored = false
		c.insert(e)
	}
}
