// Package zonecache is the always-on runnable that materialises store
// objects into serving snapshots (docs/architecture.md §3.1, §4.4, §4.5,
// decisions D7 and D33). Serving code never reads the store on the query
// path; it loads one *Snapshot per query from Cache.Snapshot and reads the
// views the registered builders derived from it.
//
// # Data flow
//
//	Store.Watch(kind) ×N ─▶ indexed object set ─(50 ms debounce)─▶ builders ─▶ atomic *Snapshot
//	                                │
//	                                └─(after every sync, then ≤ every 30 s when changed)─▶ local.db
//
// The Cache watches every kudurru.io kind (DefaultKinds) through
// store.Store.Watch. Each watch delivers the kind's current objects as
// Added events, then one Bookmark, then live events (the Watch contract of
// internal/store). When a watch channel closes, the kind is re-listed with
// a fresh Watch and objects missing from the new list are dropped at its
// Bookmark (kudurru_store_watch_resyncs_total{kind}).
//
// # Builders
//
// Consumers plug derived views in through the Builder interface. The zone
// package registers the authoritative zone trees, the policy index
// registers per-policy selections, and so on; zonecache imports none of
// them. The contract:
//
//   - Name returns a stable, unique name. It keys the view in
//     Snapshot.View and View[T], and labels
//     kudurru_zonecache_build_errors_total{builder}. Register rejects
//     duplicates.
//   - Build runs on the Cache's single rebuild goroutine, never
//     concurrently with itself or with another builder, once per published
//     snapshot: after the debounce window that follows a change, when the
//     blocking lists change (SetLists), when the serving source switches
//     between Unsynced, FromSnapshot and Live, when a record's expiresAt
//     passes, and when a builder is registered. Builders run in
//     registration order.
//   - The ObjectSet passed to Build and every object reachable from it are
//     shared and immutable: Build must not modify them. It may retain them
//     in the view it returns.
//   - The returned value is published as is to every reader; it must be
//     safe for concurrent reads and must not be modified after Build
//     returns. Build a new value each time (copy-on-write); reusing
//     unchanged parts of the previous view is encouraged (ObjectSet.Changed
//     tells which kinds changed since the previous snapshot).
//   - On error (or panic, which is recovered) the previous view of that
//     builder is kept, the other builders are unaffected, and the Cache
//     raises Degraded{BuildFailed} naming the builder until a later Build
//     of that builder succeeds. A builder that never succeeded has no view:
//     View reports ok=false.
//   - Build should finish in milliseconds; it delays every later builder
//     and the publication of the snapshot. It must not block on the Cache.
//
// # Cold start (§4.4)
//
// With Options.LocalDB set, the Cache writes the complete live object set
// and the per-kind resourceVersions to local.db after every successful
// sync and then at most every Options.PersistInterval (30 s) when it
// changed. On Start, if the store has not synced within
// Options.StartupTimeout (15 s), the Cache loads local.db and serves from
// it: State FromSnapshot, Degraded{ServingFromSnapshot},
// kudurru_store_snapshot_serving 1 and kudurru_store_snapshot_age_seconds.
// It reports ready only while the snapshot is younger than Options.MaxAge
// (24 h); once older, Snapshot.Authoritative is false and the
// authoritative stage answers SERVFAIL (never NXDOMAIN) for primary zones
// while forwarding continues. When the store syncs, the snapshot is
// swapped out for the live set. Unsynced and without a snapshot, the Cache
// is not ready and Snapshot.Authoritative is false.
package zonecache
