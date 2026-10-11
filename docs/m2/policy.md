# M2 work package: client identification, control rules, policy selection, blocking stage

Key `policy`. This package covers docs/architecture.md §7.1 steps 3, 4 and 7, §7.2, the §7.3 match reporting, the §4.1 ClientGroup/Policy/BlockList specs, D36/D58 (the zeroIP default) and D73 (list semantics). Rewrite (step 8) is v0.2 and is not part of it.

It closes these follow-up rows: "Blocking: M2 stage" and the "bootstrap `Policy/default` priority as explicit fallback" part of the objects row. The `$important`/`$badfilter` question from that blocking row was considered and stays with the v0.2 modifier slow path (§7.3). A narrower row replaces the old one.

## What was built

| Package | Contents |
|---|---|
| `internal/policy` (new) | `Build(Input, Options) *Index`, `Index.Identify/Select/SelectPolicy/GroupDecision/Diagnoses/NotLoaded/Groups`, `Decision`, `ListInfo`, `HostnameResolver`, the zonecache `Builder` (view `"policy"`), `Source`, `Static`, `FromRequest` |
| `internal/stages/identify` (new) | the `identify` stage: fills `Request.Client.GroupIDs` and `Request.Policy` |
| `internal/stages/control` (new) | the `control` stage: Allow / DropSilently / Refuse (allowed networks, blocked qtypes, malformed queries) |
| `internal/stages/blocking` (new) | the `blocking` stage, `Synthesize`, `Toggle` + `Publisher` seam, metrics, `OptionsFrom(config.Blocking)`, `Ready` |
| `internal/pipeline` (additive) | `Request.Policy any`, `ErrDrop`, a doc comment on `Client.GroupIDs` |
| `internal/config` (additive) | `blocking.enabled`, `blocking.defaultResponse`, `blocking.blockTTL`, `blocking.allowDegraded`, `blocking.customIP.v4`, `blocking.customIP.v6` (new file `blocking.go`, schema rows, `Validate` hook); drift test green |
| `NOTICE` | blocky attribution extended (client-group model, block response types, response-side CNAME check; no code copied) |

There are no changes to cmd/, docs/architecture.md, internal/blocking, internal/zonecache or any other agent's package. go.mod and go.sum are unchanged.

### Identification (`policy.Index.Identify`)

Tiers run in a fixed order, and the first tier that matches decides:

1. exact IP
2. MAC (from `Client.MACs`; this is the slot for the v0.2 inventory)
3. longest-prefix CIDR (IPv4 and IPv6 tables, probing only the prefix lengths that exist)
4. exact hostname, then nameGlob (`path.Match`; a `hostname` containing `*?[` is also treated as a glob, as in the §4.1 `"ps5*"` example), both through `HostnameResolver`
5. clientID
6. catch-all `0.0.0.0/0` and `::/0`
7. the default group (`ClientGroup default` in `Options.DefaultNamespace`)

Within a tier every matching group is joined. A tie between equal entries in different groups (same IP, MAC, hostname, clientID or equal-length CIDR) produces a `Diagnosis{Reason: Tie}` on each group involved. Addresses are unmapped (`::ffff:a.b.c.d` matches IPv4 entries in both directions), and zones are stripped. Memberships are interned at build time, so `GroupIDs` is a shared, precomputed slice. Hostname matches that join several groups are interned lazily in a `sync.Map`.

**Why catch-all CIDRs rank below hostnames and clientIDs:** the bootstrap `ClientGroup default` lists `0.0.0.0/0` and `::/0`. Taken literally, "longest-prefix CIDR before hostname" means a PS5 on the main LAN would match the /0 and could never be identified as `kids` by its hostname. Demoting /0 to its own tier keeps the documented order for every real prefix.

### Policy selection (`Index.Select`)

- **Candidates:** every Policy whose `clientGroupRefs` names one of the client's groups. Refs are resolved in the policy's own namespace, like the controller's refs.
- **Schedules:** evaluated at query time against `Request.Received`. Weekdays accept `mon`..`sun` (and full names), start/end are `HH:MM` (`24:00` is allowed as an end), and tz is an IANA zone. When `end <= start` the window crosses midnight and belongs to the weekday it starts on. `start == end` means 24 h. Empty fields mean every day, 00:00 or 24:00. A schedule that does not parse is diagnosed, and the policy then applies at all times (fail closed).
- **Merge:** policies are ordered by priority (highest first), then by `namespace/name`.
  - Lists: allow lists of every active policy, then their block lists and blocked services (`service:<id>`), de-duplicated. "Allow wins across groups" holds because `blocking.Selection` checks every allow list first.
  - Per setting (`blockMode`, `blockTTL`, `upstreamGroup`, `queryLog`, `safeSearch`, `safeSearchMode`): the first policy in that order that sets it wins.
  - `allowOnly` comes from the top policy.
- **Fallback:** when no candidate is active, `Policy default` in `Options.DefaultNamespace` applies with `Decision.Fallback = true`. When that policy is absent or out of schedule too, a shared `Decision` with `Applies() == false` is returned. It is never nil.
- **Cost:** decisions, including the `blocking.Selection` resolved against the snapshot's compiled lists, are precomputed per group set for every combination of its scheduled policies: eagerly up to 6 scheduled policies (64 decisions), lazily by bit mask up to 63, and per query beyond that. `SelectPolicy` does not allocate (`TestSelectPolicyDoesNotAllocate`).
- **List names:** each ref is resolved to the compiled list `namespace/name` first, then to the bare `name`. Either naming convention of the list compiler therefore works without coordination.
  - A ref to a BlockList that exists but is not compiled yet is reported by `NotLoaded()`, which feeds readiness.
  - Dangling refs are `MissingReference`.
  - A list referenced under the wrong field is `ModeMismatch`, and its own mode still applies.
- **Blocked services (D60):** `blockedServices` IDs resolve to the catalogue's compiled lists `service:<id>` (internal/listdata/catalogue), which the list resolver compiles into the same snapshot as the BlockLists (`listfetcher.ResolverOptions.Extra`). An ID the catalogue does not have is `UnknownService` (and the controller sets `Degraded{UnknownService}` on the Policy); a known one not compiled yet is `NotLoaded`. See docs/deploy/blocked-services.md.

### Control stage

- **Malformed queries are dropped silently:** QR set, qtype 0 or OPT, qclass 0, a question name that is not a valid FQDN, or no question. The stage returns `(nil, pipeline.ErrDrop)`, and the log observer already records StatusDropped for that case. `Options.KeepMalformed` turns this off.
- **Clients outside `Options.AllowedNetworks`** (when that list is non-empty) are refused. Loopback is always allowed. A query with no client address is refused once networks are configured.
- **Qtypes in `Options.BlockedQTypes`** are answered REFUSED with `StatusRefusedACL`, plus EDE 18 (Prohibited) when the query carried EDNS. The assignment lists only Allow/Drop/Refuse; NODATA would be gentler for clients that fail over, so it is noted as an option below.
- **Per-policy rules (v0.2)** will read `policy.FromRequest(req)`. There are no config keys yet; see the follow-ups.

### Blocking stage

- **Recursive path only.** The authoritative stage runs first and answers local zones without calling `next`, so authoritative answers never reach this stage.
- **Upstream group.** A policy's `upstreamGroup` replaces `Request.UpstreamGroup` when that is `""` or `"default"`, which is the bootstrap root zone's hint. A forward zone's own group is kept. By D82 answers forwarded to a non-default group are not cached, which is also what keeps per-policy upstream answers out of the shared cache.
- **Decision order.** The stage passes the query through when `blocking.enabled` is false, the Toggle is disabled, or the decision filters nothing. Otherwise it calls `Selection.Evaluate`:
  - allow list wins, then allow-only, then deny unless the same list's `@@` matches (D73 `ScopeList`)
  - after `next`, response-side checks run with `Selection.EvaluateResponse` over CNAME targets, SVCB/HTTPS target names, A/AAAA addresses and SVCB `ipv4hint`/`ipv6hint` (at most 64 items)
  - the response-side check is skipped after an allow-list match, as `internal/blocking` documents
  - safe search (D60), when the decision has `safeSearch: true` and no allow list matched: a search-engine host name of the catalogue is answered with a CNAME to the vendor's safe target plus the target's records resolved through the rest of the chain (or the documented addresses, for Yandex), status `blocked_safesearch`, list `safesearch:<engine>`, EDE 4 (Forged Answer), TTL as for blocks
- **Block response.**
  - Mode: the BlockList's `response` (with its own `customIP`), else the Policy's `blockMode`, else `blocking.defaultResponse`.
  - zeroIP (the default): A 0.0.0.0, AAAA ::, other types NODATA.
  - nxdomain: NXDOMAIN without an SOA.
  - customIP: the configured address, or NODATA for a family that has none.
  - refused: REFUSED.
  - TTL: the policy's `blockTTL`, else `blocking.blockTTL` (10 m), clamped to 2^31−1.
  - Header: RA set, AA clear, no authority section.
  - OPT: added only when the query had one (EDNS size 1232, DO echoed). It carries EDE 15 (Blocked) with the list name as extra text, or EDE 17 (Filtered) for allow-only blocks.
- **Status:**
  - `blocked_cname` for any response-side block, including address hits
  - `blocked_regex` for regex rules
  - `blocked_exact` for exact or suffix rules of a list made only of inline sources (the user's own list)
  - `blocked_list` otherwise
  - `Response.List` and `Response.Rule` carry the compiled list name and the rule text (§7.3 "why")
- **Temporary disable.** `Toggle` has `Disable(d)` (d ≤ 0 means until `Enable`), `Enable()`, `Enabled()` (one atomic load while on; it re-enables itself on expiry), `DisabledUntil()`, and `Apply(until)` for replicated state. `Publisher` is the documented replication seam: kube writes the `kudurru.io/blocking-disabled-until` annotation on `kudurru-runtime`, Docker writes state.db, and ha-static stays node-local. Every replica watches the field and calls `Apply`.
- **Metrics** (§11.1). Group and list label values are capped at 64; the rest become `_other`.
  - `kudurru_block_decisions_total{action,source,group}`: action is block, allow, pass or rewrite; source is deny, response, allow_only, allowlist, exception, none, disabled or safesearch (with rewrite); group is the first of the client's groups. All eight series of a group are resolved on its first query, through a copy-on-write map, so there are no label lookups per query.
  - `kudurru_block_list_hits_total{list}`.
  - `kudurru_blocking_enabled{group}`: computed at scrape time from the toggle, `blocking.enabled` and each group's current decision, so expiring disables need no timer.
- **Readiness.** `Ready()` is not ready while `Index.NotLoaded()` is non-empty, unless `allowDegraded` (default true), matching §5.2.

## Wiring for serve

Stage order is fixed by `pipeline.Order`: **ingress → identify → control → authoritative → blocking → cache → upstream**, with the log observer as before.

```go
import (
    "github.com/robertherbaugh/kudurru/internal/policy"
    blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
    "github.com/robertherbaugh/kudurru/internal/stages/control"
    "github.com/robertherbaugh/kudurru/internal/stages/identify"
)

// after zc := zonecache.New(...) and zc.Register(zoneview.Builder(logger)):
if err := zc.Register(policy.Builder(policy.Options{
    DefaultNamespace: bootstrap.DefaultNamespace, // the namespace passed to bootstrap.Seed (release namespace in k8s)
}, logger)); err != nil {
    return err
}
polSrc := policy.Source{Cache: zc}

bopts := blockingstage.OptionsFrom(cfg.Blocking)
bopts.Index, bopts.Registerer = polSrc, reg
// bopts.Toggle = blockingstage.NewToggle(nil); toggle.SetPublisher(...) once replication exists (M3)
bs, err := blockingstage.New(bopts)
if err != nil {
    return err
}
checker.Register("blocking", bs.Ready)

stages := []pipeline.Stage{
    &ingress.Stage{NodeID: cfg.Node.Name, Hostname: hostname, Version: info.Version},
    identify.New(polSrc, nil),
    control.New(control.Options{}), // AllowedNetworks / BlockedQTypes from config once keys exist
    authoritative.New(zoneview.Source{Cache: zc}),
    bs,
}
// then cache (optional) and upstream exactly as today
```

- **Lists:** blocking applies only the lists that reach `zc.SetLists(*blocking.Snapshot)`. Nothing calls `SetLists` in serve today; the listfetcher/list-compiler package owns that. Until it is wired, policies resolve no lists (`NotLoaded`, Info log), `allowDegraded: true` keeps the node ready, and only allow-only policies block. Compiled list names may be either `namespace/name` or the bare name.
- **dnsserver:** to make DropSilently truly silent, `Handler.Resolve` should send nothing when `errors.Is(err, pipeline.ErrDrop)`. Until then such queries get SERVFAIL, as for any error (follow-up row).
- **Query log:** `Request.Client.GroupIDs` and `policy.FromRequest(req)` (with `Decision.QueryLog`, `Decision.Policy`) are available to the log observer for "client group" columns and per-policy log exclusion; `stages/log` does not read them yet.

## Measurements

Shared 8-CPU sandbox (other agents running), Go 1.27.2, `-benchtime 1s -count 3`, median shown.

| Benchmark | Result |
|---|---|
| `SelectPolicy`, exact-IP client | 31 ns, 0 allocs |
| `SelectPolicy`, catch-all client | 45 ns, 0 allocs |
| `SelectPolicy`, CIDR client | 57 ns, 0 allocs |
| `SelectPolicy`, CIDR client with a scheduled (Europe/Berlin) policy | 73 ns, 0 allocs |
| identify stage `Serve` | 59 ns, 0 allocs |
| Stage overhead, 3 groups, 5 lists (≈17k entries), client with 4 block + 1 allow list: baseline chain (terminal stage only) | 174 ns, 2 allocs |
| … + identify + control + blocking, query passes (question check + response-side check of one A record) | 597 ns, 5 allocs → **≈ 0.42 µs added** |
| … + identify + control + blocking, query blocked (zeroIP synthesis) | 845 ns, 11 allocs → ≈ 0.67 µs added |
| `Build`: 100 groups, 1,100 matchers, 100 policies | 1.0 ms, 12k allocs |

- The goal of < 1 µs added per query is met.
- The 3 extra allocations on the pass path are the pipeline's per-stage `next` closures (24 B each), not the new stages.
- Profile of the pass path: the matcher chain `Selection.Evaluate` takes ≈ 150 ns, the response-side `inspect` + `EvaluateResponse` ≈ 80 ns, control's `dns.IsDomainName` ≈ 45 ns, identify ≈ 60 ns, and the rest is pipeline dispatch.

Tests:

- **Identification matrix:** 18 cases, covering tie diagnosis, IPv4-mapped forms, hostname/glob/clientID/MAC tiers, catch-all and the default group.
- **Schedules:** a 24-case matrix in Europe/Berlin, America/New_York and UTC, covering DST, crossing midnight, 24-hour windows and `24:00`, plus invalid schedules.
- **Policy merge:** the priority merge, scheduled policies with the fallback, many scheduled policies (the lazy path), and the "no policy applies" case.
- **Lists and builder:** list resolution and diagnoses; the builder through a real zonecache on the memory store, including the `SetLists` rebuild and reuse of the Index on zone-only changes.
- **Blocking decisions:** a 24-case matrix over every response shape, EDE presence and code, statuses and response-side CNAME/A/AAAA/HTTPS target/ipv4hint checks.
- **Config and wire:** TTL defaults and overrides, plus a wire round-trip of EDE.
- **Toggle and metrics:** toggle expiry, forever and replication `Apply`; metrics, including the scrape-time gauge.
- **Control:** a 17-case matrix.
- **End to end:** ingress → identify → control → blocking → fake upstream, with an observer.

`make test` passes for the whole module with `-race`. One run hit a pre-existing timing flake in `internal/controller` `TestKubeDriver` (the SerialBumped event list was read before the final event arrived under load). It passed on rerun, and the package is untouched. `golangci-lint run ./...` and `go vet ./...` are clean.

## Surprises

- **Catch-all /0 versus the documented tier order.** The bootstrap default group is two /0 CIDRs, which would make hostname and clientID matchers dead under a literal "CIDR before hostname" order. The /0 entries got their own tier (see above).
- **The builder could not live in zonecache.** The assignment suggested a policy-index builder in a new zonecache file. That would make zonecache import internal/policy, which imports zonecache for `ObjectSet`: an import cycle. It also contradicts zonecache's "imports none of them" contract. The builder lives in `internal/policy/view.go`, as `zoneview` does for zones, and zonecache is untouched. `policy.Build` takes a plain `Input`, so tests need no `ObjectSet` constructor.
- **No list compiler is wired yet.** `ObjectSet.Lists()` is nil in serve today, so until the listfetcher lands blocking applies no list in production (see Wiring).
- **`nxdomain` with or without SOA.** The assignment says SOA-less, while §7.1 says "with SOA". The block response follows the assignment: there is no zone to take an SOA from, and an invented SOA would be cached by clients for its minimum. Worth a decision row.
- **Silent drop needs a transport change.** `pipeline` had no way to say "do not answer". The additive `ErrDrop` sentinel is the smallest contract; dnsserver needs a one-line check to honour it.
- **customIP needs addresses at the config level.** §6.2 allows `defaultResponse: customIP` but has no address keys. `blocking.customIP.v4/v6` were added (validated; customIP without either is rejected).

## Proposed decisions (for the integrator's decision-log pass)

1. Identification tiers as listed above, with /0 demoted to a catch-all tier, a tie meaning "join all tied groups plus a diagnosis", and the default group as the final fallback.
2. Policy merge: union of lists, per-setting highest priority, `allowOnly` from the top policy, ties by `namespace/name`. Policy `default` applies as the fallback when no candidate policy is active.
3. Invalid schedules fail closed (the policy applies at all times).
4. Block response precedence: BlockList `response`, then Policy `blockMode`, then `blocking.defaultResponse`. `nxdomain` has no SOA. EDE 15 is used for list blocks and EDE 17 for allow-only. The EDE extra text is the list name.
5. A policy `upstreamGroup` replaces only the default-group hint. Such answers are not cached (follows from D82).
6. `pipeline.ErrDrop` is the silent-drop contract.
7. Settings keys `blocking.enabled|defaultResponse|blockTTL|allowDegraded|customIP.v4|customIP.v6`.

## Follow-ups

These were added to docs/followups.md:

- `$important`/`$badfilter` in the v0.2 modifier slow path.
- dnsserver honouring `pipeline.ErrDrop`.
- Control config keys (`control.allowedNetworks`, `control.blockedQTypes`), and a `HostnameResolver` source (PTR cache, DHCP leases, UniFi) for hostname/nameGlob matchers.
- Toggle replication: a Publisher for the kube ConfigMap and state.db, a watcher calling `Toggle.Apply`, and `POST /dns/blocking`.

Not added, for the integrator:

- The log observer could record `GroupIDs`, `Decision.Policy`, and honour `Decision.QueryLog=false`.
- Blocked-qtype answers could become configurable (REFUSED vs NODATA).
- ~~Safe-search (v0.2) reads `Decision.SafeSearch`.~~ Shipped in v0.1 (D60): the blocking stage rewrites the catalogue's search-engine host names when `Decision.SafeSearchOn()`, after the allow lists and the block lists, with status `blocked_safesearch` (docs/deploy/blocked-services.md).
