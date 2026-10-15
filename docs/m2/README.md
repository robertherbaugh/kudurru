# Milestone M2: authoritative + blocking

These reports record the M2 implementation and measurements before
v0.1.0. For the release's behavior and limitations, read the
[architecture tour](../architecture/README.md) and [changelog](../../CHANGELOG.md).

Five work packages, wired in `cmd/kudurru/serve.go` (commit "feat(serve): wire milestone M2"). Decisions D86 to D97 in [architecture.md](../architecture.md) record what changed; the M1 carry-overs from [followups.md](../followups.md) landed in the hardening package.

| Package | Report | Headline |
|---|---|---|
| Transfers, TSIG, NOTIFY, D80 gate | [xfr.md](xfr.md) | AXFR of 100k records in 50-80 ms; IXFR fallback; TSIG on every miekg transport; NOTIFY with retries; shared contentHash between controller and zone cache |
| Policy, identify, control, blocking | [policy.md](policy.md) | 0.4-0.7 µs added per query with 3 groups and 5 lists; identification tiers, policy merge, block responses with EDE 15/17, silent drops |
| Leader-fetched blocklists + internal listener | [listfetcher.md](listfetcher.md) | 1 M-entry replica pull in 90-237 ms over the HKDF-derived TLS listener; leader compile 0.7 s; embedded MIT starter list |
| Hardening carry-overs | [hardening.md](hardening.md) | +53 % UDP throughput with SO_REUSEPORT sockets; 127 ns rate-limit check; cache dump/restore of 200k entries in 0.3 s; slow-watcher conformance case; binary-size budget in CI |
| SQLite query log | [querylog.md](querylog.md) | 28-31 k rows/s write capacity; 141 B/row; rollups, retention, privacy modes, live tail and the read API the dashboard needs |

Verified live from one config: an adblock rule answers 0.0.0.0 with EDE 15 and the list name, a file-managed zone transfers over TCP, querylog.db and the list cache are on disk, the starter list compiles on first boot.
