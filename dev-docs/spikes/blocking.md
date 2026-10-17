# Spike (d): blocking engine core

Key `blocking` · week-1 spike · 2026-10-11 · architecture §7.1 step 7, §7.2, §7.3, §13.1 (M2), §14 risk 7.

## Verdict

**Pass.** All three §7.3 gates hold on the spike machine with headroom, on synthetic data and on 3.85 M real list entries:

| Gate (§7.3) | Limit | Measured (median of 5 processes) | Result |
|---|---|---|---|
| p99 lookup, 1 M-entry suffix set, single-threaded | < 2 µs | hit **1179 ns**, miss **705 ns** (synthetic); hit 1055 ns, miss 655 ns (953 k real entries) | pass |
| RSS holding 3 M entries | < 150 MB | **90.3 MB** steady state (peak during the build 138.2 MB); 89.8 MB for 3.85 M real entries | pass |
| Compile 1 M entries | < 5 s | **0.84 s** adblock, 0.76 s hosts (parse + compile from text) | pass |
| +20 % regression fails the build | — | `make bench` + `internal/blocking/benchgate`; catches a removed bit filter (+124 to +286 %) and passes against itself | in place |

The p99 figures include about 22-38 ns of timer overhead (`BenchmarkLatency/timer`).

The risk that remains is **regex lists**, not the suffix set. Before I added the gram index below, 100 regexes cost 1.1 µs per miss and 1000 cost 19 µs. Now indexed regexes cost O(name length). Each expression *without* a usable literal still costs 25-75 ns per lookup, so the realistic policy fixture (3 M entries plus 100 regexes, 10 of them unindexed) has a miss p99 of 2.4 µs. Recommendations are below.

## What was built

All code is in `internal/blocking` (≈6.7 k lines including tests; coverage 94-98 %). The only module change is `golang.org/x/net` moving from indirect to direct (for IDNA). It is cgo-free, and no miekg types are used.

| Package | Contents |
|---|---|
| `blocking/matcher` | `Exact`: length-bucketed sorted concatenated strings, binary search. `Suffix`: TLD buckets of the same layout plus a maphash bit filter over the full base names; walks parent labels broadest-first and returns the matched base as a substring of the query. `Regex`: per-list hard cap, with a required-literal gram index. `IPSet` for response-side address entries. Every lookup is zero-allocation (`TestLookupsDoNotAllocate`). |
| `blocking/parser` | Clean-room parser for `auto/hosts/domains/adblock/regex`. Includes format detection, HTML/binary refusal, `! Title:`/`# Title:` extraction, CRC-32 of normalised rule lines, and `Checksum` without parsing. Modifiers are tokenised and kept (`UnsupportedRule.Modifiers`). Has a fuzz target. |
| `blocking` | `List` (three matchers plus an IP set, mode block/allow, and an exception chain for in-list `@@`); `Compile` with the CRC-32 skip; immutable `Snapshot` with `Contains(name) → map[list]rule`, `ContainsAllow`, `Rebuild` (keeps the old list on failure); `Selection.Evaluate` / `EvaluateResponse` returning a zero-allocation `Decision` that maps onto `pipeline.Status`; `SpecFromAPI` / `SelectPolicy` glue to `api/v1alpha1`. |
| `blocking/internal/synth` | Seeded generator of realistic names (23.2 B average) and regex shapes. |
| `blocking/benchgate` | The regression gate (median of runs, +20 %, per-unit noise floors, absolute limits). |
| `bench/` | `baseline.txt` (recorded with `make bench-baseline`) and `limits.txt` (the §7.3 limits). |

### Seams for the M2 stage

The blocking stage (not part of this spike) is meant to work like this:

1. On every snapshot swap, compute `snap.SelectPolicy(&policy.Spec, blocking.ScopeList)` once per policy.
2. Per query, call `sel.Evaluate(qname)` with zero allocations:
   - `ActionBlock`: synthesise the block response and set `resp.Status = d.Status()`, `resp.List = d.List`, `resp.Rule = d.Rule()`.
   - `ActionAllow` with `ReasonAllowList`: skip the response-side checks.
3. After `next`, call `sel.EvaluateResponse(cnameTargets, answerAddrs)`. The stage extracts the names and addresses from `dns.Msg` (CNAME targets, SVCB/HTTPS targets and address hints, A/AAAA), so miekg types stay in the stage.

`$important` is the one §7.1 rule this spike does not apply, because the assignment puts it in the unsupported bucket.

### Decisions (to ratify in §16)

| Topic | Decision | Why | Rejected |
|---|---|---|---|
| Hosts/domains entries | exact match only; a per-source `HostsSubdomains` option turns them into suffix entries | The AdGuard KB, Pi-hole and blocky all treat them as exact; "domains with subdomains" lists exist (OISD/hagezi variants) | subdomains by default |
| `*.x`, `\|\|*.x^` | match `x` **and** its subdomains (same as `\|\|x^`) | blocky's semantics, which the `*.domain` list formats target; one suffix semantics | literal adblock reading (subdomains only) |
| `\|\|x` (no `^`) | read as `\|\|x^` | DNS lists never mean the browser substring match (`x.evil`, `xample.organic`) | substring semantics or the unsupported bucket |
| `@@` scope | an exception cancels deny rules **of its own list** (`ScopeList`, default); `ScopeSelection` gives AdGuard Home's merged-engine behaviour | one list's fixes cannot silently unblock another list; use allow-mode lists for that | global scope by default |
| Allow lists | entries (with or without `@@`) are allow entries; allow wins across lists, then allow-only, then exception, then deny | §7.1 step 7, blocky | — |
| Hosts addresses | only `0.0.0.0`, `::`, `127/8`, `::1` block; other addresses count as unsupported `hosts-rewrite` (AdGuard Home answers with them) | rewrites are v0.2 | blocky's "any IP blocks" |
| Hosts boilerplate | `localhost`, `broadcasthost`, `ip6-*` and the like are counted as ignored, never blocked | otherwise every StevenBlack-style file blocks `localhost` | blocking them |
| Bare IPs, `\|\|1.2.3.4^` | `KindIP` entries matched against answer addresses | §7.1 response-side checks | ignoring |
| Modifiers | all eight DNS modifiers (`important, badfilter, dnstype, dnsrewrite, client, ctag, denyallow, respgeo`), mid-pattern wildcards, unanchored and prefix patterns, CIDRs and Pi-hole `;querytype=` go to the unsupported bucket with the parsed modifiers kept; unknown modifiers, cosmetic rules and URL rules are invalid | spec: an unknown modifier means the whole rule is ignored | dropping silently |
| Regex case | regexes run on the lower-cased name; an expression with upper-case letters compiles with `(?i)` | names are normalised | case-sensitive misses |
| Names | trailing dot dropped, ASCII lower-cased, Unicode converted to A-labels (UTS #46 lookup, `_` allowed); 63-byte labels, 253-byte names; an all-numeric TLD is rejected | wire names are A-labels | — |

Clean room: the adblock grammar comes from the public AdGuard KB page (fetched 2026-10-11) and nothing else. I did not read or import AdGuard Home or urlfilter code. I studied blocky v0.35.0 (Apache-2.0, fetched from proxy.golang.org) for the string-cache design, and NOTICE records that.

## Measurements

Machine: `Intel(R) Xeon(R) Processor @ 2.90GHz` VM, 8 vCPU (the Go runtime saw 6 Ps), 55 MB LLC, 16 GB RAM, Linux 6.18.49, `go1.27.2 linux/amd64`, `CGO_ENABLED=0` for builds. Lookups are single-threaded (one benchmark goroutine).

### Commands

```sh
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
go test -race -count=1 ./internal/blocking/...          # unit, golden, conformance, race
go test -run NONE -fuzz FuzzParse -fuzztime 60s ./internal/blocking/parser/
make bench-baseline     # 5 fresh processes x all benchmarks -> bench/baseline.txt (3 min 9 s)
make bench              # same run, gated against bench/baseline.txt + bench/limits.txt
KUDURRU_BLOCKING_BENCH_LISTS=/tmp/blk-real/porn.txt:/tmp/blk-real/malware.txt:/tmp/blk-real/ads.txt \
  go test -run NONE -bench External -benchmem -count 3 ./internal/blocking/
```

### Test output

```
ok   github.com/robertherbaugh/kudurru/internal/blocking                 1.230s   coverage: 97.9%
ok   github.com/robertherbaugh/kudurru/internal/blocking/benchgate       1.023s   coverage: 94.5%
ok   github.com/robertherbaugh/kudurru/internal/blocking/internal/synth  1.074s   coverage: 93.8%
ok   github.com/robertherbaugh/kudurru/internal/blocking/matcher         3.015s   coverage: 94.7%
ok   github.com/robertherbaugh/kudurru/internal/blocking/parser          1.081s   coverage: 97.5%
fuzz: elapsed: 1m0s, execs: 1246800 (66863/sec), new interesting: 578 (total: 654) -- PASS
golangci-lint run ./...  ->  0 issues;  go vet ./...  ->  clean;  go test -race ./... (whole repo) -> ok
```

The first fuzz run found an empty regex line (`//` in regex format), which would have matched every name. It is fixed and kept as a seed in `parser/testdata/fuzz`.

### Lookup latency (`bench/baseline.txt`, median of 5 processes)

| Fixture | Query | p50 | p99 | p99.9 | mean (no timer) |
|---|---|---|---|---|---|
| suffix-1M (1 M `\|\|x^`) | hit (½ subdomains) | 568 ns | **1179 ns** | 1534 ns | 594 ns |
| | miss | 170 ns | **705 ns** | 1117 ns | 141 ns |
| exact-1M (1 M hosts) | hit | 569 ns | 1108 ns | 1448 ns | 576 ns |
| | miss | 109 ns | 540 ns | 869 ns | 86 ns |
| policy-3M (1 M suffix + 2 M exact + 1 k allow + 100 regex, 5 lists) | hit | 832 ns | 1596 ns | 2304 ns | 881 ns |
| | miss | 1184 ns | 2433 ns | 5628 ns | 1228 ns |
| StevenBlack sample (2,852 real) | hit | 162 ns | 248 ns | 360 ns | — |
| | miss | 82 ns | 273 ns | 530 ns | — |
| regex-100 (10 unindexed) | miss | 905 ns | 1732 ns | 2601 ns | — |
| regex-500 (50 unindexed) | miss | 3320 ns | 6846 ns | 12414 ns | — |
| regex-1000 (100 unindexed) | miss | 6202 ns | 12661 ns | 19020 ns | — |
| timer overhead | — | 22 ns | 38 ns | 52 ns | — |

Every `Evaluate` benchmark reports `0 B/op 0 allocs/op`.

### Real lists (local only, not committed)

Three Block List Project lists were used: Unlicense, commit `ee3bdbad`, `porn.txt` + `malware.txt` + `ads.txt`, 3,845,114 entries averaging 18.3 bytes. Medians of 3 runs:

| Fixture | Query | p50 | p99 | Without the fence index |
|---|---|---|---|---|
| exact, 3 lists, 3.85 M | hit | 666 ns | 1311 ns | p50 827, p99 1720 |
| | miss | 136 ns | 626 ns | p50 143, p99 811 |
| suffix (`porn.txt`, 953 k as `HostsSubdomains`) | hit | 497 ns | **1055 ns** | p50 571, p99 1166 |
| | miss | 164 ns | **655 ns** | p50 175, p99 779 |

| Memory, real 3.85 M | rss | peak | heap | engine B/entry | compile |
|---|---|---|---|---|---|
| all exact | 89.8 MB | 157.7 MB | 77.3 MB | 20.3 | 1.64 s |
| first list as suffix | 91.1 MB | 150.1 MB | 77.6 MB | 19.5 | 1.74 s |

### Memory, 3 M synthetic entries

The 3 M entries are 1 M adblock suffix, 1 M hosts and 1 M domains, at 23.2 B average. They are compiled by `Rebuild` from files in a fresh child process (`TestMemoryHelper`), then `runtime.GC` and `debug.FreeOSMemory` run and `/proc/self/status` is read:

```
BenchmarkMemory/3M-6  1  2327814414 ns/op  8.715 base-rss-MB  2.313 compile-s  24.31 engine-B/entry  78.51 heap-MB  138.2 peak-rss-MB  90.57 rss-MB
```

The medians are rss **90.3 MB**, peak 138.2 MB, heap 78.4 MB, base 8.8 MB, and 24.3 engine bytes per entry (name bytes plus 1.5 B filter plus 0.5 B fence). Before collecting between lists in `Rebuild`, peak was 146-150.5 MB.

### Compile

```
BenchmarkCompile/adblock-1M-6    2   843924103 ns/op  32.08 MB/s  1000000 entries  121457308 B/op  141142 allocs/op
BenchmarkCompile/hosts-1M-6      2   755803294 ns/op  42.44 MB/s  1000000 entries  152875540 B/op    2708 allocs/op
BenchmarkCompile/unchanged-1M-6 19    62725199 ns/op  431.7 MB/s                      65680 B/op       8 allocs/op
```

When the CRC-32 of every source's rule lines is unchanged, `Compile` returns the previous `List` after one checksum pass. That costs 63 ms per 1 M lines (27 MB) and allocates nothing proportional to the list. Comment-only changes such as `! Last modified:` also skip the rebuild (`TestCompileChecksumSkip`).

### Gate behaviour

`make bench` against a fresh baseline on the same machine: `benchgate: ok`. Most metrics land within ±10 %. The largest swings were exact-1M miss p50 at +20 % (+22 ns, under the 25 ns floor) and StevenBlack miss p99 at +42 % (+114 ns). The second one failed the first self-check, which is why p99 now has its own 150 ns floor.

To validate the gate, I disabled the bit filter (`mayContain` always true) and ran the suffix benchmarks:

```
regression  BenchmarkLatency/suffix-1M/hit     p99-ns   1179   1725   +46.3%
regression  BenchmarkLatency/suffix-1M/miss    ns/op    255.1  665.8  +161.0%
regression  BenchmarkLatency/suffix-1M/miss    p50-ns   170    545    +220.6%
regression  BenchmarkLatency/suffix-1M/miss    p99-ns   705    1582   +124.4%
regression  BenchmarkEvaluate/suffix-1M/miss   ns/op    140.8  543.5  +286.0%
benchgate: FAIL
```

### Correctness corpus

`testdata/conformance` holds 10 scenarios and 144 expectations covering:

- `||x^` subdomain semantics versus exact matches (`notexample.org`, `example.org.evil.com`, `example.organic`)
- `|x^` and plain domains
- trailing dots, case and IDNA (`bücher` → `xn--bcher-kva`, `MÜNCHEN` → `xn--mnchen-3ya`)
- 63-byte labels and 253/254-byte names
- `@@` in list scope and selection scope; allow-wins; allow-only
- hosts exactness, sink and non-sink addresses, boilerplate, multiple hosts per line, inline comments, `hostsSubdomains`
- `*.x` (apex included, marked as a deviation from AdGuard Home), mid-pattern wildcards
- adblock regexes and the Pi-hole regex format
- every modifier
- cosmetic and URL rules
- response-side CNAME and IP checks

`TestRealCorpus` compiles the StevenBlack sample against a golden file: 3,277 lines, 2,854 rules, 2,852 entries, 0 invalid, `crc32=b648c645`. It checks every entry and that no subdomain matches.

## Surprises

1. **The worktree branch started at the initial commit**, not the scaffold. I fast-forwarded it to `79a0e37` before starting (no rebase, nothing else touched).
2. **blocky v0.35.0 has no "TLD-bucketed sorted sets plus a maphash bit filter"**: its wildcard cache is a trie with a map per node (`trie/trie.go`), and the tree has no maphash. The design §7.3 attributes to blocky (and `research/blocking-engines.md` §2.5 describes) is therefore kudurru's own. NOTICE credits blocky only for what was really adapted: the exact-set layout, the cheapest-first chain and matched-rule reporting.
3. **Regexes dominate lookup cost.** Before the gram index, a linear scan with a `strings.Contains` prefilter cost 1.1 µs per miss at 100 regexes and 19 µs at 1000. That pushed the policy fixture's miss p99 to 4.5 µs. Indexing each expression under one 4-byte gram of its required literal (the §7.3 item 4 "shortcut index" idea, reimplemented) cut 1000 indexed regexes to a **140 ns** median.
4. **Go's regexp cannot help with unindexed expressions.** I tried a single alternation of the unindexed expressions as a negative pre-check: it is 4-13x *slower* than separate calls (Go's regexp has no DFA). Unindexed expressions cost 25-75 ns each, so I added `Stats.RegexUnindexed` and a warning above 50.
5. **The fence index's value depends on the name-length distribution.**
   - 2x on the first synthetic set (17.5 B names, large buckets)
   - 5-10 % on the final synthetic set (23 B names, spread over more buckets)
   - 20-25 % at p99 on real lists (18.3 B, concentrated)

   It costs 0.5 B/entry, so I kept it.
6. **`-count N` inside one process understates the variance.** Memory layout differs between processes: the same benchmark moved 25-31 % between two processes while samples within each process agreed. `make bench` therefore runs each count in a fresh process. Tail percentiles also need a larger noise floor (150 ns for p99) than medians (25 ns).
7. **Zero-copy line parsing cut allocations 8-370x but barely moved peak RSS.** That removed 1.14 M → 141 k allocs per 1 M-line adblock compile and 1.0 M → 2.7 k for hosts. Peak is dominated by arena growth for the list in flight, so collecting garbage between lists in `Rebuild` helped more (-10 MB).
8. Synthetic names (23.2 B) are longer than both real samples (StevenBlack 22.8 B, Block List Project 18.3 B), so the synthetic memory numbers err on the high side.
9. Commit trailers use the harness-configured `Co-Authored-By: Claude Opus 5.5` line rather than the `Claude Fable 5.1` line in the task text.

## Recommended changes to dev-docs/design-record.md

I have not made these edits myself.

1. **§7.3 item 2**: drop "copied from blocky" for the suffix set. blocky v0.35 uses a trie, and the TLD-bucket plus bit-filter design is kudurru's. Mention the fence index (8 B per 16 records) and the shared per-process maphash seed that lets one hash serve every list's filters.
2. **§7.3 item 3**: replace "warning above 500 regexes" with:
   - a required-literal gram index;
   - a warning above **50 unindexed** regexes (each costs about 25-75 ns per lookup);
   - a hard cap default of **1000** per list (`blocking.DefaultRegexHardCap`, configurable).

   Optionally make the 500-total warning informational.
3. **§7.3 gate wording**: define "RSS" as steady state after a GC, measured in a fresh process. Also track peak during the build: 138 MB for 3 M synthetic, about 150-158 MB for 3.85 M real. Note that a copy-on-write `Rebuild` keeps the old snapshot alive, so a full refresh needs roughly old + new + one list in flight (about 2x plus 50 MB). Either recommend per-list publication (swap after each changed list, as blocky does per group) or document the sizing.
4. **§7.3 / §12 `make bench`**: a committed baseline only means something on one runner class. Record it with `make bench-baseline` on that runner (or self-hosted hardware), run counts in separate processes, and consider an A/B job (main versus PR on the same runner) instead of a committed baseline once CI exists.
5. **§7.1 step 7 / §16**: record the decisions table above, especially `@@` list scope (with `ScopeSelection` as the AdGuard Home option), `*.x` including the apex, `||x` read as `||x^`, and non-sink hosts addresses treated as rewrites.
6. **`$important` and `$badfilter`** are cheap in this design: `$important` is a deny chain checked before exceptions, and `$badfilter` removes the identical rule before compiling. Consider moving them from the v0.2 slow path into v0.1, because §7.1 already promises "`$important` beats `@@`".
7. **API (`api/v1alpha1`)**, owned by another package:
   - add `BlockListStatus.unsupported` (and `regexes`/`regexUnindexed` or a condition message) so lists with features that are not compiled are visible;
   - add a `ListSource.subdomains: bool` for "domains with subdomains" lists, mapping to `Source.HostsSubdomains`.
8. **§7.3 "Contains(name) → map[list]rule"**: keep it for explain and the UI, and name `Selection.Evaluate` (zero allocation, first deciding list) as the query-path API.

## Refreshing the baseline

```sh
make bench-baseline          # runs 5 fresh processes, checks bench/limits.txt, writes bench/baseline.txt
git add bench/baseline.txt   # commit together with the change that moved the numbers
```

- Refresh on the same runner class that runs `make bench`. `benchgate` warns when the baseline's `cpu:` line differs from the current run, and `-strict-cpu` turns that into a failure.
- Refresh when `benchgate` reports `improved` (more than 20 % better), so later regressions are measured from the new level.
- Adding a benchmark shows it as `new` until the baseline is refreshed. Removing one fails the gate as `missing` until the baseline is refreshed.
- Knobs: `BENCH_COUNT` (default 5), `BENCH_THRESHOLD` (default 20), `BENCH_PKGS`. `bench/limits.txt` holds the absolute §7.3 limits and the metrics excluded from the relative comparison.
- To check real lists without committing them, set `KUDURRU_BLOCKING_BENCH_LISTS` to `file1:file2:...` and run `go test -run NONE -bench External ./internal/blocking/`.

## Follow-ups

- Cross-check the conformance corpus black-box against an AdGuard Home container (§7.3; needs Docker). The deviations are listed in `testdata/conformance/README.md`.
- M2 blocking stage: `SelectPolicy` per snapshot, extracting response-side names and addresses from `dns.Msg`, EDE 15/17, and `blocked_exact` for inline lists.
- List fetcher: leader download, digest pull, and the `sha256(url)` disk cache feeding `FileSource`. Refuse HTML/binary bodies at fetch time as well (`parser.ErrHTML` / `ErrBinary`).
- Move `benchgate` to `hack/` or `tools/` once cache benchmarks join `make bench`, and add a CI job on a fixed runner class.
- Lower peak build memory if needed: chunked arenas (no growth copies) or per-list snapshot publication.
- Run the fuzz target in CI (`FuzzParse`) per §12.
