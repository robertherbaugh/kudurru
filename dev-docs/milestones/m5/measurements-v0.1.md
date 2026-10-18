# v0.1 measurements: load, upgrade and soak

Measured on 2026-10-12 before the first release candidate, with kudurru in
Docker as a user runs it. The runs found five bugs, all fixed with
regression tests before the numbers below were final (see "Bugs found").

## Setup

| | |
|---|---|
| Host | cloud VM, 8 vCPU Intel Xeon 2.90 GHz, 16 GB, Linux 6.18, Docker 25.0 |
| kudurru | the release Dockerfile image at the branch head (60.5 MB), `--read-only`, `--cap-drop ALL`, `no-new-privileges`, a fresh named volume, pinned to CPUs 0-3 |
| Load | dnsperf 2.14 in its own container on CPUs 4-7, 4 threads, 64 clients |
| Upstream | dnsmasq with `--cache-size=0` answering every name with 192.0.2.1, on the host loopback (shares CPUs 4-7 with dnsperf) |
| Lists | a 1,000,000-entry hosts list from a local HTTP server and the built-in starter list, both on the default group's policy |
| Zone | one primary zone with 5 A records |
| Config | query log on (`privacy: full`), rate limiting off (loopback is exempt anyway), log level warn |

Query files: **cached** (hot names), **local** (the zone's records),
**blocked** (names on the 1M list), **miss** (2M unique names, every query
forwarded), **mix** (1M queries: hot, cold, blocked and local names; 252k
distinct names).

## Startup

| | |
|---|---|
| `docker run` to ready (`/healthz/ready`) | 0.44 s |
| `docker run` to the 1M-entry list answering 0.0.0.0 | 2.6 s (fetch, parse, compile, swap) |
| Idle with the 1M list | 108-129 MB RSS, 113 goroutines, 47 fds |

## Throughput at saturation (4 CPUs)

30 s each; no query was lost in any run.

| Query file | q/s | Avg latency | CPU |
|---|---|---|---|
| cached | 142,600 | 0.69 ms | 4.0 cores |
| local (authoritative) | 149,400 | 0.66 ms | 4.0 |
| blocked | 138,700 | 0.71 ms | 4.0 |
| mix | 72,200 | 1.38 ms | 4.0 |
| miss (every query forwarded) | 39,800 | 2.50 ms | 4.0 |

A forwarded query costs about 100 µs of CPU, a cached one about 28 µs. The
miss figure is after the query-log fix below (29,700 q/s before it).

## Latency at fixed rates (mix)

| Rate | Avg latency | Std dev | CPU | Query log |
|---|---|---|---|---|
| 10,000 q/s | 0.082 ms | 0.116 ms | 1.0 core | every query logged |
| 25,000 q/s | 0.114 ms | 0.415 ms | 2.4 cores | every query logged |
| 50,000 q/s | 0.497 ms | 0.816 ms | 2.9 cores | 31% shed |

The query log takes about 26,000-34,000 events/s on this host. Above that
the collector's queue sheds events (`kudurru_querylog_dropped_total{stage="queue"}`)
by design, and DNS answering is not slowed.

## Docker networking

| Path | cached q/s | mix q/s | mix at 25k q/s |
|---|---|---|---|
| `--network host` | 142,600 | 72,200 | 0.114 ms |
| bridge, published ports, client to the host address (iptables DNAT, the LAN path) | 153,300 | 80,600 | 0.122 ms |
| bridge, published ports, client to 127.0.0.1 (docker-proxy) | 116,100 | | |

Published ports cost nothing measurable for LAN clients. Through the
loopback, docker-proxy is the limit (kudurru used 3.2 of its 4 cores).

## Real upstreams

200 q/s for 60 s over 768 real names from the catalogue (70% A, 30% AAAA):

| Upstreams (parallel_best) | Avg latency | Result |
|---|---|---|
| 1.1.1.1, 9.9.9.9 (plain) | 1.55 ms | 11,999 NOERROR, 1 SERVFAIL (both upstreams timed out once) |
| tls://1.1.1.1, tls://9.9.9.9 | 1.70 ms | 12,000 NOERROR |

The DoT run showed 335 REFUSED answers from 1.1.1.1 that Quad9 covered. On
its own, Cloudflare's DoT endpoint (1.1.1.1 and 1.0.0.1) answers a burst of
about 30 queries on one TLS connection and then REFUSED (no EDE) until the
rate drops; a new connection answers at once, and paced at 20 q/s per
connection nothing is refused. With only `tls://1.1.1.1` configured, about a
third of the names in a burst failed with SERVFAIL. Cloudflare DoH and plain
DNS, and Quad9 DoT, refused nothing. Fixed: a REFUSED answer from a DoT
server is asked again once on a fresh connection; afterwards 150 of 150
burst names resolved through Cloudflare DoT alone. This was seen from a
cloud address; a residential address may get a different limit.

## Memory

Live heap after a forced GC, profiled under a unique-name flood: 133 MB
(DNS cache 200k entries ~45 MB, query-log queue ~30 MB, response buffers).
RSS runs higher because of GC headroom and the query log's memory-mapped
database. 15 minutes into the soak (1.8M rows), RSS was 801 MB: 300 MB
anonymous (the Go runtime, `go_memstats_sys_bytes` 312 MB, flat) and
466 MB of clean file-backed pages of querylog.db. Each SQLite connection
maps up to `mmap_size` = 256 MiB of the file and Linux counts every mapping,
so RSS grows with the database up to about connections × 256 MiB plus the
anonymous part. The kernel reclaims those pages under memory pressure; size
container memory limits by the anonymous part plus a margin, not by RSS. The
cache is bounded (200k entries here,
evictions by capacity), and goroutines and fds stay flat under load.

## Upgrade

The oldest on-disk format (98fca6b: query-log schema 1, unblock requests
not yet a kind, allow and deny lists as inline lines) was populated by that
binary: an API token, a zone with 20 records, a 1M-entry list, a policy,
exact and regex allow entries, a deny entry, and 310 logged queries. The
head binary was then started on the same data directory:

| Check | Result |
|---|---|
| Starts, ready | 0.20 s |
| Old API token and the admin password | work |
| Zone records, DNS answers (blocked, allowed, denied, regex allow, zone) | unchanged |
| Query-log totals and rows | carried over; old blocked rows keep their list |
| Allow and deny lists | migrated to structured entries (log: `allow and deny lists migrated to structured entries lists=2`) |
| Unblock requests API, policy state endpoint, new writes | work |
| ERROR lines in the log | none |

16 of 16 checks pass (a script outside the repository drove the API and dig
against both binaries).

## Bugs found

| Bug | Fix |
|---|---|
| A fresh Docker named volume at `/var/lib/kudurru` (the documented `docker run` and every compose file) came up root-owned and kudurru exited with `open state.db: permission denied` | 2884a2b: every image has `/var/lib/kudurru` and `/run/kudurru` owned by 65532; the ko images build on `kudurru-base`; `make volume-check` (D154) |
| The pending hour's per-domain counts grew by every unique name, about 2 GB an hour at the query log's peak rate under a random-subdomain flood (TCP is not rate limited), and were copied on every stats read | d7302fd: capped at 100,000 names; past the cap the hour is read from the fact table |
| A deleted user could be put back into a node's auth cache by the background `lastLoginAt` update and stay valid until the next re-watch (the CI flake in `TestRecreatedUserDoesNotInheritCredentials`) | b82153d: the cache keeps a deletion's resourceVersion for a minute and refuses older copies |
| `POST /zones/{zone}/records` sometimes answered 409 to one client writing records in sequence (the controller's status write raced the API's read-modify-write) | d32837d: re-read and re-apply up to three times |
| Cloudflare DoT refusals reached clients as SERVFAIL | 6b340db: a DoT REFUSED is asked again on a fresh connection |

## Known limits

- Stats reads (`/stats/summary`, `/stats/top`) over a window that includes
  the current hour take longer as the hour fills once it has more than
  100,000 distinct names: at 2,000 q/s with the mix file, 13 ms at 4k rows,
  0.8 s at 480k rows and 1.8 s at 1M rows, dropping when the hour is rolled
  up. Below the cap (a home network) the in-memory counts answer. DNS is not
  affected. Follow-up: per-domain rollups for shorter buckets
  (dev-docs/followups.md).
- The query log keeps up with about 26,000-34,000 queries/s per node on
  this host and sheds the rest.

## Soak

Started 2026-10-12 05:12 UTC: the same image, a 2 GiB query-log cap
(`querylog.retention.maxDBBytes`, so the size trimming runs within hours),
mix at a steady 2,000 q/s, and a sampler every minute (RSS, heap,
goroutines, fds, query-log size, rows and drops, CPU, dig latency, and the
time of `/stats/summary`, `/queries` and `/stats/top`).

First 15 minutes (1.8M rows, 242 MB database): goroutines 104-105, fds
39-42, no query-log drops, CPU 40-50% of one core, heap in use 160-210 MB,
anonymous memory 300 MB. Stats reads over the filling hour reached 3.2 s
(see "Known limits").

Results are added here when the run ends.
