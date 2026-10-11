# M2 work package `xfr`: zone transfers, NOTIFY, TSIG and the D80 gate

Packages: `internal/zone`, `internal/stages/authoritative`,
`internal/zoneview`, `internal/notifier` (new), `internal/dnsserver`
(`tsig.go`, `Options.TSIG`/`OnTSIGFailure`, the miekg handler),
`internal/controller` (hash, validator, conflict conditions), small
additive changes in `internal/pipeline`, `internal/config` and
`api/v1alpha1` (reason constants only). Architecture references: §4.1
ZonePolicy, §4.4, §4.6, §7.1 step 6, §13.1 M2, D12, D80.

## What was built

### AXFR and IXFR out (RFC 5936, RFC 1995)

- `zone.Zone.AXFR(maxBytes) iter.Seq[[]dns.RR]` walks the zone tree in
  canonical order (owner, type, canonical rdata) with the SOA first and
  repeated last, in chunks whose uncompressed size stays within
  `zone.DefaultChunkBytes` (64 KiB minus 1 KiB for the header, question,
  OPT and a TSIG with a SHA-512 MAC). The zone is immutable, so the
  sequence is lazy and can be re-iterated. Occluded and glue data are
  included, as RFC 5936 §3.3 requires.
- The authoritative stage answers AXFR and IXFR (`transfer.go`) after
  `queryAccess`:
  - Nothing is transferred unless the D80 gate is open. Otherwise the
    answer is SERVFAIL `servfail_unsynced`.
  - `Zone.TransferAllowed(client, tsigKey)` applies `zoneTransfer`, which
    is the same six-way vocabulary as `queryAccess` with
    `transferACL` and the zone's NS glue plus `status.nameserverAddresses`.
    An empty value means deny. When `transferTSIGKeyRefs` is non-empty, the
    request must also be signed with one of those keys. To allow
    key-only transfers from anywhere, use `allow` plus a key. A refusal is
    REFUSED with `refused_acl`.
  - AXFR is served over TCP and DoT only. Over UDP, DoH, DoQ and DoH3 it
    is REFUSED.
  - IXFR without a journal (v0.2):
    - A secondary whose serial is current or newer (RFC 1982) gets the
      single SOA.
    - Over TCP, an older serial gets the whole zone in AXFR form
      (RFC 1995 §4).
    - Over UDP, an older serial gets the single SOA, so the secondary
      retries over TCP (§2).
    - An IXFR without the authority SOA gets FORMERR.
  - The answer is streamed through the new `pipeline.Response.Transfer`
    (`iter.Seq[*dns.Msg]`). `Msg` is the first message, for observers.
    Every message has AA set, echoes the question and OPT, and is
    compressed.
- `kudurru_zone_transfers_total{zone,direction="out",result}` counts
  transfers. The results are `success`, `uptodate`, `tcp_required`,
  `refused`, `unsynced`, `formerr` and `aborted`. A transfer counts once
  its last message is written, or as `aborted` when the client leaves.
- `Stage.Transfer(zone, serial)` implements `TransferProvider` with the
  same chunking and no policy check. It returns a buffered and closed
  channel, so a consumer that stops early leaks no goroutine.
  `ErrTransferNotImplemented` is gone. The errors are now
  `ErrZoneNotFound`, `ErrNotTransferable` and `ErrUnsynced`.

### TSIG (RFC 8945)

- The seam is `dnsserver.TSIGKeyring`
  (`Lookup(name) (secret []byte, algorithm string, ok bool)`).
- The v0.1 implementation is `StaticKeyring`, built by
  `KeyringFromConfig(cfg.TSIGKeys)`. Each `secretFile` holds the base64
  secret in tsig-keygen format. Key names compare canonically.
- `TSIGProvider(keyring)` adapts the keyring to miekg's `TsigProvider`
  with HMAC-SHA256 and HMAC-SHA512. A wrong algorithm for a key is
  BADKEY.
- Every udp/tcp/dot `dns.Server` gets the provider (one line in `Bind`).
  Without a keyring, every key is unknown. A provider is always installed
  because miekg skips verification without one and would report a forged
  signature as verified.
- `serveMiekg` checks `TsigStatus` before the pipeline. A failure is
  answered NOTAUTH with the TSIG error:
  - BADKEY and BADSIG replies are unsigned (§5.3.2).
  - BADTIME replies are signed, carry the request's time signed, and put
    the server clock in Other Data (§5.2.3).
  - `Options.OnTSIGFailure(reason)` reports `badkey`, `badsig` or
    `badtime`.
- A verified request carries the key in the new
  `pipeline.Client.TSIGKey`. Every reply message is signed with that key.
  In a transfer stream, the first message is signed with the request MAC
  and the rest timers-only, each chained to the previous MAC (§5.3.1).
  UDP truncation leaves room for the TSIG record.
- Inbound AXFR, IXFR, NOTIFY and UPDATE (and plain queries) are all
  verified. NOTIFY and UPDATE then get NOTIMP, signed (v0.2).
- DoH, DoQ and DoH3 do not verify TSIG. A signed request there is
  treated as unsigned, so it cannot obtain a transfer.
- `config.tsigKeys` (a list of `{name, algorithm, secretFile}`) is
  `hostOnly`, `writeOnly` and `restartRequired`. It is validated for
  names, algorithms (hmac-sha256, hmac-sha512), secret files and
  duplicates. The schema drift test is green.

### NOTIFY out (RFC 1996): `internal/notifier`

- `notifier.New(Options)` returns a leader-only runnable
  (`NeedLeaderElection() == true`, `Run(ctx)`).
- It reads the served zone set: the same `authoritative.Source`, through
  `zoneview.Source`. A primary zone with `notify != none` is notified
  when its served serial differs from the last one notified. The served
  serial is `status.serial`, as stamped by the leader.
- Every zone is notified once when a term starts, as BIND does on load.
  `SkipInitial` turns that off.
- Nothing is notified while the gate is not open.
- `Trigger()` (hook it to `zonecache.OnPublish`) makes the reaction
  immediate. A 1 s poll is the fallback.
- Targets:
  - `notifyTargets` (`ip`, `ip:port`, `[v6]:port`; port 53 by default).
  - The apex NS hosts except the SOA MNAME, which is the RFC 1996 §2
    notify set. Their addresses come from in-zone glue, or from
    `Options.Resolver` (the default is `net.DefaultResolver`).
  - Duplicates are removed.
- `zone.SendNotify` sends one NOTIFY over UDP:
  - opcode NOTIFY, AA set, the SOA question and the new SOA in the answer
    section (§3.7);
  - from `Options.SourceAddr` (the `notify.source` setting);
  - signed with the zone's first `transferTSIGKeyRefs` key, in which case
    the reply must be signed and verify;
  - when the zone names a key the node does not have, nothing is sent
    (result `nokey`).
- Retries:
  - up to `Attempts` (5) per target, with exponential backoff (2 s
    doubling, capped at 60 s) and a 3 s timeout per attempt, while the
    target stays silent or the network fails;
  - an error rcode or a TSIG failure is final;
  - a newer serial cancels the round in flight for that zone (result
    `superseded`), so bursts coalesce.
- `kudurru_zone_notify_total{zone,result}` counts final results:
  `success`, `timeout`, `network`, `rcode`, `tsig`, `nokey` and
  `superseded`.

### The D80 refinement

- `zone.Set` gains `Gate() zone.Gate`. `Synced()` is still
  `Gate() == GateOpen`.
- `GateUnsynced` (nothing loaded) keeps the M1 rule:
  - every query is SERVFAIL, including names under no zone and forward
    zones;
  - positive data a loaded zone asserts is still served.
- `GateStale` (a local.db snapshot older than maxAge) answers SERVFAIL
  for every name under a primary zone, whatever the data. Forward zones
  forward with their group hint, and names under no zone continue down
  the pipeline.
- The new status `servfail_unsynced` (29, still within the 32-value
  budget) replaces `servfail_upstream` for these answers. `Upstream` is
  `zonecache:unsynced` or `zonecache:stale`.
- `zoneview.Source.Zones()` maps the zonecache state onto the gate
  (`zoneview.Gate(snap)`) with `Snapshot.WithGate`. That call shares the
  zones and does not allocate on the live path.

### Controller: one hash, one parser, the zone model's conflicts

- **Hash and record count.** `status.contentHash` and `recordCount` now
  come from `zone.Build(dz, zone.ServedRecordSets(dz, sets))`, the same
  call the zone cache (`zoneview.Builder`) makes on every replica.
  - `ServedRecordSets` applies `recordSetSelector`, drops record sets
    that are invalid as a whole, and orders the rest oldest first.
  - The leader therefore stamps exactly the hash of the zone it serves:
    the canonical RR form, the SOA fields and the apex NS included, the
    serial excluded (D12).
  - `zoneview` previously ignored the selector and the creation-time
    order. Not-selected record sets were served, and duplicate ownership
    could disagree with the controller. Both are fixed by sharing the
    selection.
- **Validator.** `Options.Validate` defaults to `zone.ValidateEndpoint`,
  the zone parser. `ValidateEndpoint` remains for anyone who wants the
  miekg-free one.
- **Identity.** Endpoint identity is the controller's rule in both
  places: owner, type and set identifier.
  - Different set identifiers union into one RRset.
  - Disabled, expired and view-scoped endpoints keep their identity.
  - Type mnemonics are case-insensitive.
- **CNAME exclusivity.** A CNAME conflicting with data from another
  object is `Duplicated` on the later object. A conflict within one object
  is `Invalid`.
- **DNAME occlusion.** Data below a DNAME owner, or a DNAME above
  existing data, is `Occluded` (new `v1alpha1.ReasonOccluded`, RFC 6672
  §2.4) and left out of the zone.
- **Where conflicts surface.** Record-set conditions and Events
  (`Occluded` is a new Event reason), the zone's Degraded summary and
  the hash all reflect these conflicts. `zone.ErrorReason` classifies
  build errors for this.
- **Bad DNSZone spec.**
  - A DNSZone that does not build (bad zone name, queryACL or
    nameservers) is Ready=False Invalid.
  - Errors in the transfer and notify parts of the policy fail closed:
    transfers are denied, a bad notify mode is none, and bad targets are
    dropped. The zone is still served, and Degraded=True Invalid reports
    the errors (`Errors.Policy`).
- **Build caching.** Zone builds parse every record, so they are cached
  per zone version and made by the zone reconcile (one at a time,
  coalesced). Record-set reconciles read the latest build, and the zone
  reconcile re-queues the record sets whose conflicts changed. The first
  version, which built per record-set reconcile, starved the memory
  driver's watch under `-race` (see Surprises).
- **Reasons.** `AsExpected`, `ZoneNotFound`, `NotSelected`,
  `MissingReference` and `Unsupported` moved to `api/v1alpha1`. The
  controller constants alias them.

### Bench gate

- `make bench` also runs `^BenchmarkLookup100k$` of `internal/zone` in a
  fresh process per count.
- `bench/baseline.txt` holds five recorded runs.
- `bench/limits.txt` caps p99 at 2 µs and allocations at 3 per lookup,
  and ignores B/op (the benchmark's latency recorder).
- Checked:
  - `benchgate` passes on the recording;
  - a forged 2.7 µs p99 fails;
  - the baseline compared with itself passes.

## Wiring for `kudurru serve` (cmd/ is the integrator's)

```go
// TSIG keys (tsigKeys) and the transfer metrics.
keyring, err := dnsserver.KeyringFromConfig(cfg.TSIGKeys)
if err != nil { return err }
tsigFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
    Name: "kudurru_dns_tsig_failures_total", Help: "Requests rejected for their TSIG (RFC 8945), by error.",
}, []string{"error"})
reg.MustRegister(tsigFailures)

// Authoritative stage with transfers counted.
auth := authoritative.New(zoneview.Source{Cache: zc})
auth.Metrics = authoritative.NewMetrics(reg)
stages := []pipeline.Stage{ingressStage, auth /* , cache, upstream */}

// Listeners verify and sign TSIG.
registry, err := dnsserver.NewRegistry(dnsserver.Options{
    Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, Proxy: proxyPolicy,
    TSIG:          keyring,
    OnTSIGFailure: func(reason string) { tsigFailures.WithLabelValues(reason).Inc() },
})

// NOTIFY out: leader-only, triggered by every published snapshot.
var notifySrc netip.Addr
if cfg.Notify.Source != "" {
    notifySrc = netip.MustParseAddr(cfg.Notify.Source) // validated by config.Load
}
nt, err := notifier.New(notifier.Options{
    Source: zoneview.Source{Cache: zc}, Keyring: keyring, SourceAddr: notifySrc,
    Registerer: reg, Logger: logger,
})
if err != nil { return err }
zc.OnPublish(func(*zonecache.Snapshot) { nt.Trigger() })   // before zc.Start
runner.Add("notifier", true, nt.Run)                        // leader-only, next to "controller"

// Controller: nothing to add. Options.Validate defaults to zone.ValidateEndpoint
// and the hash is the zone model's.
```

`zoneview.Builder(logger)` and `zoneview.Source{Cache: zc}` are unchanged
call sites. The TSIG counter could also live in `internal/metrics.DNS`
next to `Dropped`.

## Contract changes (shared packages)

- **`pipeline`** (all additive):
  - `StatusServfailUnsynced` = 29 (`servfail_unsynced`, class error);
  - `Client.TSIGKey string`;
  - `Response.Transfer iter.Seq[*dns.Msg]`.

  No existing field changed.
- **`zone.Set`** gains `Gate()`. Every implementation lives in the owned
  packages (Snapshot, zoneview).
- **`zone.Errors`** gains `Policy`, and `Empty()` includes it.
- **Endpoint identity in `zone.Build`** is behaviour, not API:
  - set identifiers no longer collide;
  - disabled endpoints claim their identity;
  - CNAME conflict and occlusion messages carry a reason prefix (the
    existing substrings are kept).
- **`authoritative`**:
  - `ErrTransferNotImplemented` is removed;
  - unsynced answers carry `StatusServfailUnsynced`;
  - AXFR/IXFR answer per policy instead of NOTIMP.

  Three existing tests changed accordingly: the axfr/ixfr golden cases
  now expect REFUSED (`zoneTransfer` defaults to deny), the unsynced
  status changed, and `TestTransferSlot` now expects `ErrUnsynced`.
- **`controller`** tests that computed the expected `status.contentHash`
  with the M1 tuple hash now compute it with `zoneHash`. That helper
  builds the expected records as a zone, so the assertion is as strict as
  before (any extra or missing record changes it). `ContentHash` (tuples)
  and its golden test are kept. The `invalid target` case finds
  "not an IPv4 address" because the zone parser now says that for A and
  AAAA.
- **`config`**: `tsigKeys`, `notify.source`.
- **`api/v1alpha1`**: reason constants only.

## Measurements

8 shared CPUs (GOMAXPROCS 6), four other agents running, Go 1.27.2.

| Scenario | Result |
|---|---|
| AXFR of 100,000 records + NS over TCP loopback (`dns.Transfer` client, `KUDURRU_MEASURE=1 go test -run TestMeasureAXFR100k ./internal/dnsserver`) | 100,003 RRs in 69 messages, ~2.9 MB compressed; unsigned 50-67 ms, TSIG hmac-sha256 60-81 ms (1.2-2.0 M RRs/s) |
| BenchmarkLookup100k baseline medians (ns/op, p50, p99, allocs) | hit 447 / 365 / 720 ns / 1; cname 712 / 625 / 1128 / 2; wildcard 510 / 410 / 1049 / 3; nodata 351 / 283 / 462 / 0; nxdomain 279 / 214 / 352 / 0 (M1: 428 / 349 / 695 for hit) |
| authoritative.Serve before → after (gate and transfer branch) | authoritative answer 421 → 408 ns, NXDOMAIN 390 → 381 ns, not local 158 → 161 ns; allocations unchanged (5 / 4 / 1) |
| `zoneview.Source.Zones()` | live path returns the built snapshot, 0 allocations; stale or unsynced costs one 40 B snapshot header per query |
| Controller burst, 1,000 record-set creates (memory driver) | coalescing off: converged after 45-58 ms, 1.007-1.010 writes per change; 250 ms coalescing: 205-210 ms, 1 serial bump, 1.001 writes per change |
| Controller paced updates (each converges first) | Apply → zone status p50 1.9 ms, p99 3.6 ms (M1: 0.41-0.53 / 1.1 ms). The zone reconcile now builds and hashes the real 1,000-record-set zone (p90 2.3 ms). Record-set reconcile p50 21 µs; new term over 1,000 converged sets 27 ms, 0 writes |
| NOTIFY | delivery to a local secondary in under 1 ms; a retry after silence costs one timeout plus backoff (tests use 150 ms / 10 ms) |

## Tests

- **`dnsserver`**:
  - AXFR end to end with `dns.Transfer` (TSIG verified on every message)
    over the real registry, with hmac-sha256 and hmac-sha512;
  - 64 KiB framing read from the raw TCP stream;
  - BADKEY, BADSIG and BADTIME replies, including the server clock and
    MAC size, the hook, and no pipeline involvement;
  - policy over the wire: an ACL zone, an unsigned request to a keyed
    zone, UDP, IXFR current and IXFR fallback;
  - a signed UDP query and NOTIFY get signed replies;
  - keyring loading and algorithm mismatch.
- **`authoritative`**:
  - the D80 matrix: 6 query kinds × open/unsynced/stale;
  - the transfer policy table (13 cases);
  - chunking, abort counting, OPT echo, and the `TransferProvider`
    capability.
- **`zone`**:
  - chunking and stream order;
  - RFC 1982 arithmetic;
  - the transfer ACL matrix over six modes, keys and v4-mapped clients;
  - notify policy, and policy errors failing closed;
  - DNAME occlusion in both orders, at the apex and at the owner;
  - CNAME conflict reasons and identity rules;
  - gates, `ServedRecordSets` and `ValidateEndpoint`;
  - `SendNotify` against a fake secondary: success, signed,
    REFUSED, silence, unsigned reply, wrong secret.
- **`notifier`**:
  - TSIG-verified delivery to a fake secondary;
  - a new serial notified once;
  - retry after two silent attempts, and giving up after `Attempts`;
  - nokey, gated sets and notify none;
  - notify-set targets: MNAME excluded, glue, resolver, deduplication;
  - superseded rounds, `SkipInitial`, defaults.
- **`controller`**:
  - CNAME and DNAME conditions with Events and the hash without them;
  - re-queue when a conflict clears;
  - invalid zone vs policy-degraded zone;
  - an apex NS change moves the serial;
  - the default validator is the zone parser.
- **`zoneview`**:
  - controller ↔ zone cache hash parity over one memory store, through
    selector exclusions, invalid and duplicate sets, CNAME and DNAME
    conflicts, disabled endpoints and an SOA change;
  - the stale local.db gate end to end (a store that never syncs, a 25 h
    old snapshot): primary SERVFAIL, forward zone forwards with its
    group, other names forward.
- **`config`**: `tsigKeys` and `notify.source` decode, defaults and
  validation.

`make test` (race, envtest) is green for the whole module. golangci-lint
reports nothing on the touched packages.

## Surprises

- The worktree branch started at the initial commit and was
  fast-forwarded to e56cbea.
- **`zoneview` diverged from the controller.** It built zones from all
  record sets in namespace/name order. Not-selected record sets were
  served, and the earlier object in creation time could lose a
  contested identity to a lexically smaller one, which contradicted the
  controller's Duplicated condition. Parity required sharing the
  selection, not just the hash.
- **Building the zone per record-set reconcile** made a 1,000-object
  burst ~10x heavier, so the memory driver's watch fell behind and
  silently dropped events (the open follow-up about slow watchers). The
  test hung on a 896-record view. The fix was builds owned by the zone
  reconcile. The unmodified M1 tree also times out in that burst once
  `make test` is loading the machine, so that follow-up matters.
- **miekg does not verify TSIG without a provider.** With no
  `TsigProvider`, `TsigStatus()` is nil for a signed request, which reads
  as "verified". Every server therefore gets a provider, even with no
  keys.
- **miekg will not verify a NOTAUTH reply.** `stripTsig` returns
  `ErrAuth` for that rcode. The BADTIME test checks the MAC size, Other
  Data and time signed instead.
- **miekg's server sets no write deadlines.** A transfer to a client
  that stops reading holds the connection. The fix belongs in the
  listener code the hardening package is rewriting (follow-up).
- **Two small parser gaps.** Lower-case record types (`"a"`) were
  rejected by the zone parser but accepted by the controller, and the
  zone parser's A/AAAA error was "bad A A". Both are fixed.

## Recommended architecture changes (not applied)

1. §3.3 `Request`/`Response`: add `Client.TSIGKey` and
   `Response.Transfer iter.Seq[*dns.Msg]` (the multi-message answer
   stream). The capability list keeps
   `Transfer(zone, serial) (<-chan []dns.RR, error)`, with buffered
   channels.
2. §11.2 taxonomy: add `29 servfail_unsynced`.
3. D80: record the refinement as implemented:
   - unsynced: everything SERVFAIL, positive data from loaded zones
     still served;
   - stale local.db: every name under a primary zone SERVFAIL, forward
     zones and other names forwarded;
   - `Upstream` is `zonecache:unsynced` or `zonecache:stale`.
4. New decision: `status.contentHash` is `zone.ContentHash` of
   `zone.Build(ServedRecordSets(...))`, shared by the controller and the
   zone cache. Identity is (owner, type, setIdentifier). Different set
   identifiers union into one RRset. Unserved endpoints keep their
   identity.
5. New decision: `zoneTransfer` semantics:
   - the address mode AND (when set) `transferTSIGKeyRefs`;
   - empty means deny;
   - errors fail closed;
   - AXFR over TCP/DoT only;
   - IXFR answered by AXFR until the journal exists;
   - NOTIFY is signed with the first `transferTSIGKeyRefs` key.
6. New decision: invalid transfer and notify policy degrades a zone
   instead of taking it down. queryACL errors still do, since failing
   open would leak the zone.
7. §4.1 internal kinds: `TSIGKey` sits behind `dnsserver.TSIGKeyring`;
   v0.1 reads `tsigKeys` from the config file (D85 settings list:
   `tsigKeys`, `notify.source`).

## Follow-ups

These are recorded in docs/followups.md:

- per-message write deadlines on transfer streams;
- transfer chunks budgeted by compressed size;
- requeueing at `NextExpiry`;
- the controller parse cache;
- a NOTIFY key field and a per-zone notify source;
- the store-backed `TSIGKey` kind, and TSIG on DoH/DoQ/DoH3;
- the doctor self-NOTIFY.

These rows are closed:

- the D80 refinement;
- the controller canonical hash, validator and CNAME exclusivity;
- the DNAME occlusion condition;
- the `servfail_unsynced` status;
- the zone bench gate;
- the reason constants (the `BlockListStatus`/`ListSource` schema items
  remain).
