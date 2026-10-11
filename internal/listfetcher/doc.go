// Package listfetcher implements "fetch once, serve everywhere" for
// blocklists (docs/architecture.md §3.1 listfetcher row, §4.6, §7.3,
// decision D15).
//
// Two halves share one DiskCache (blocking.listsDir, default
// <dataDir>/lists):
//
//   - Fetcher, leader-only: for every BlockList source (URL, or
//     configMapRef through a ConfigMapFunc), a conditional GET
//     (If-None-Match / If-Modified-Since) on the list's refresh interval,
//     the body tee'd into a temporary file while it is hashed, size-capped
//     (maxHTTPSize), checked by the blocking parser (HTML, binary, empty,
//     mostly invalid) and renamed over lists/<sha256(url)> atomically.
//     Transient failures are retried with jittered exponential backoff,
//     then the source cools down; the previous body always stays in
//     service. Downloads run with bounded concurrency. The fetcher then
//     writes only status.sources[].{name,digest,etag,entries,invalid,
//     lastFetched,error}, status.entries, status.ruleCount, status.invalid,
//     status.unsupported and status.lastFetched through Store.UpdateStatus,
//     re-reading the object and retrying on conflicts so the controller
//     keeps observedGeneration and the conditions.
//   - Resolver, every node: turns the BlockLists and their published
//     digests into bodies (the local cache by digest, else
//     GET /internal/lists/{digest} from the leader with the sha256
//     verified, else after FallbackAfter its own download), compiles them
//     with blocking.Rebuild (unchanged lists are reused by checksum) and
//     hands the snapshot to zonecache.Cache.SetLists. Init strategies fast,
//     blocking and failOnError follow blocky.
//
// The list pipeline (conditional GET, sha256(url) disk cache, tee-to-temp
// with atomic rename, retry and cooldown, maxErrorsPerSource,
// keep-old-on-failure, bounded concurrency, init strategies) follows the
// design of blocky's lists package (Apache-2.0, see NOTICE); no blocky
// code is copied. maxErrorsPerSource deliberately differs: a body is
// refused only when its invalid lines exceed the limit and also outnumber
// its rules, because kudurru counts cosmetic rules of browser lists as
// invalid and still uses their DNS rules (§7.3).
package listfetcher
