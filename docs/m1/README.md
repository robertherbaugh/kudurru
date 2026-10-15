# Milestone M1: resolver + store

These reports record the M1 implementation and measurements before
v0.1.0. For the release's behavior and limitations, read the
[architecture tour](../architecture/README.md) and [changelog](../../CHANGELOG.md).

Six work packages, each with its own report, wired together in `cmd/kudurru/serve.go` (commit "feat(serve): wire milestone M1"). Decisions D79 to D85 in [architecture.md](../architecture.md) record what changed.

| Package | Report | Headline |
|---|---|---|
| sqlite Store driver | [store-sqlite.md](store-sqlite.md) | Conformance suite unchanged; Apply 0.34 ms p50; Get 15 µs; 10k List 80 ms; hash-chained audit in the same transaction; schema refusal and migrations |
| Zone model + authoritative stage | [zone.md](zone.md) | CoreDNS-style tree plus exact index; wildcards, CNAME/DNAME, ACLs, SERVFAIL-while-unsynced; BIND import/export round-trips; 100k-record lookups sub-microsecond |
| zonecache | [zonecache.md](zonecache.md) | Store watches to immutable snapshots with typed builders; local.db cold start verified on envtest; 10k objects rebuild timing |
| Cache + upstream groups | [cache-upstream.md](cache-upstream.md) | dnsproxy groups with parallel_best/strict/random/rtt_weighted, conditional forwarding, ECS policy; sharded TTL cache with prefetch, serve-stale and coalescing |
| Controller + cluster runner | [controller.md](controller.md) | One reconciler per kind, serial stamping per D12 with 250 ms coalescing, Events; runner with the D44/D78 election contract |
| File objects + bootstrap | [objects.md](objects.md) | objects.d loader with fsnotify, diagnoses instead of aborts, ownership rules (D84); first-boot seeding |

Verified live from one config: a file-managed primary zone answers with AA, wildcard CNAME chase, NXDOMAIN with SOA, forwarding over dnsproxy, a cache hit on the second query, state.db on disk, leader election and first-boot seeding.

At the end of M1, the carry-overs were multi-socket SO_REUSEPORT listeners,
cache Dump/Load, the primary-only unsynced gate (D80) and the REST API.
Do53 multi-socket listeners, cache persistence and the unsynced gate landed
in [M2](../m2/README.md); the API landed in [M3](../m3/README.md).
Multi-socket encrypted listeners remain planned for v0.2.
