// Package cache is kudurru's response cache (dev-docs/design-record.md §3.3
// "internal/cache"): a sharded LRU of packed DNS messages keyed by query
// name, type, class, DO and CD bits and ECS subnet, with
//
//   - per-entry expiry plus a serve-stale window (RFC 8767) and a
//     failure-recheck hold after every upstream failed;
//   - prefetch accounting: a fresh hit on an entry with at least
//     PrefetchThreshold hits within PrefetchWindow and less than a tenth of
//     its TTL (at least 1 s) left hands the caller a one-time refresh claim;
//   - in-flight coalescing of identical misses (singleflight);
//   - Dump/Load of the live entries as opaque bytes for the local.db
//     shutdown hook.
//
// The package is DNS-agnostic beyond Key: TTL clamping, negative caching,
// OPT stripping and the TTL countdown live in the cache pipeline stage
// (internal/stages/cache), which owns the miekg/dns types.
//
// The design follows blocky's caching resolver (Apache-2.0; prefetch by hit
// count within a window, stored packed messages, never caching
// conditional/local answers) with an own sharded LRU; the serve-stale
// timers are the RFC 8767 recommendations as listed in §3.3.
package cache
