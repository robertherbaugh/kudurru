# M1 work package `zone`: zone model and authoritative stage

Packages: `internal/zone`, `internal/stages/authoritative`, plus one field
on `pipeline.Request`. Architecture references: §3.3 (`internal/zone`
bullet), §4.1 (DNSZone, DNSRecordSet, Endpoint), §4.2, §7.1 step 6, D12,
D28.

## What was built

### `internal/zone`

- **Endpoint parsing** (`ParseEndpoint`, `ParseTarget`, `OwnerName`,
  `TTL`). Targets are parsed with `dns.NewRR` against the zone origin and
  default TTL. Supported types: A AAAA CAA CNAME DNAME HTTPS MX NAPTR NS
  PTR SRV SSHFP SVCB TLSA TXT, plus `TYPEnnn` with RFC 3597 rdata. SOA,
  meta and DNSSEC types are refused even as `TYPEnnn`. Names are absolute
  with or without a trailing dot (the external-dns convention), and `@` is
  the apex. Unquoted TXT values count as one raw string (external-dns
  writes them that way). Errors are `field.ErrorList`s with paths such as
  `spec.endpoints[3].targets[1]`, ready for a condition.
- **Builder / Build**. Builds a zone from a DNSZone and DNSRecordSets.
  Endpoints with `disabled`, an `expiresAt` that has passed, or any
  `view` are skipped (D28). Views are v0.2; until then a view-scoped
  record is withheld rather than served to everyone. `Zone.NextExpiry()`
  tells the zone cache when to rebuild. Each endpoint is all-or-nothing.
  The builder enforces CNAME exclusivity (RFC 1034 §3.6.2), no CNAME or
  NS at the apex (apex NS comes from `spec.nameservers`), and
  `Duplicated` on the later object (or union merge with
  `BuildOptions.MergeUnion`). The SOA is built from `spec.soa` with
  defaults, the serial from `status.serial` and negative TTL
  min(TTL, minimum) (RFC 2308). Every RRset gets one TTL, the minimum of
  its records (RFC 2181 §5.2), and canonical rdata order.
- **Tree**. A left-leaning red-black tree with one node per owner name,
  keyed by a precomputed canonical key: reversed labels, lowercased,
  escape-decoded and separated by 0x00. Plain byte comparison of two keys
  gives RFC 4034 §6.1 order, and an ancestor's key is a prefix of every
  descendant's. Empty non-terminals are materialised. The design follows
  CoreDNS `plugin/file/tree` (NOTICE). An exact-match hash index over the
  same nodes serves lookups. The tree serves ordered walks: RRsets,
  export, the content hash, and later NSEC.
- **Lookup** (`Zone.Lookup`, `Zone.LookupFrom`). Returns the
  answer/authority/additional sections and a `Kind`:
  - `Success`, `NoData`, `NXDomain` (SOA in authority).
  - `Delegation` (child NS plus in-zone glue, AA clear; DS at a cut is
    answered from the parent side).
  - `CNAME`: the chain left the zone or ran into a cut. `Target` holds the
    name to continue with.
  - `Loop`: more than `MaxChain` = 20 hops. The hop budget is shared
    across zones through `LookupFrom`.
  - `YXDomain`: a DNAME result longer than 255 octets.

  Wildcards follow RFC 4592 (closest encloser, owner replaced by the
  qname). DNAMEs follow RFC 6672 (the DNAME plus a synthesised CNAME).
  The additional section carries in-zone A/AAAA for NS, MX and SRV
  targets. CAA, HTTPS and SVCB pass through unchanged. Zones without
  delegations or DNAMEs below the apex take a single-probe fast path.
  `TestLookupFlatMatchesWalk` checks it against the full walk.
- **Hash and serial**. `Zone.ContentHash()` is `sha256:` over every
  served RR in canonical order (owner key, type, class, TTL, canonical
  rdata with names lowercased per RFC 4034 §6.2), with the SOA serial
  zeroed. `NextSerial(prev, now)` is D12's `max(prev+1, unix_now)` with
  RFC 1982 wrap. `StampSerial(prevSerial, prevHash, newHash, now)` is
  the leader's decision, returning `(serial, changed)`.
- **Journal seam**. `Delta`, `Journal` (Append/Since/Truncate) and
  `ErrJournalGap`. No implementation, since IXFR is v0.2. `Diff(prev,
  next)` already computes the delta a journal would store.
- **BIND I/O**. `ImportBIND(r, origin)` turns a master file into
  `Imported` (a DNSZoneSpec plus Endpoints plus warnings). Its behaviour:
  - SOA and apex NS map onto spec fields.
  - DNSSEC records are skipped with a warning, and other types without a
    mnemonic become `TYPEnnn`.
  - `$INCLUDE` is refused (miekg's default).
  - A file may hold at most `MaxImportRecords` = 1,000,000 records.

  `Imported.Objects` splits the result into a DNSZone and DNSRecordSets
  of at most 1000 endpoints each. `ExportBIND(w, z)` writes `$ORIGIN`,
  `$TTL`, the SOA, then every RR in canonical order with absolute names.
  Export → import → build gives the same content hash, and a second
  export is byte-identical (tested and fuzzed).
- **Routing and snapshots**. `Set` is the read interface (`Synced`,
  `Match`). `Snapshot` is an immutable implementation with longest-suffix
  `Match` (root `.` allowed, no allocation) and copy-on-write
  `With`/`Without`/`WithSynced`. `Holder` wraps an
  `atomic.Pointer[Snapshot]` and satisfies the stage's `Source`.
- **ACLs**. `ACL`/`ParseACL` handle ordered CIDR or address entries with
  `!` negation, where the first match wins and the default is deny. They
  are reused for `transferACL` in M2. All six `queryAccess` modes are
  implemented. `allowOnlyZoneNameServers` uses the in-zone glue of the
  apex NS plus `status.nameserverAddresses`.

### `internal/stages/authoritative`

`pipeline.Stage` named `authoritative`. For each query:

1. NOTIFY and UPDATE get NOTIMP. Handlers can be installed in
   `Stage.Notify` / `Stage.Update` for v0.2. Other opcodes, other
   classes and multi-question messages go to `next`.
2. No snapshot, or one that is not synced: SERVFAIL (`Upstream:
   "zonecache:unsynced"`). This covers names under no local zone too,
   because a zone that has not loaded yet could cover them. Positive data
   (`Success`, `Delegation`) from a loaded zone is still served.
3. Longest-suffix zone match. No match passes to `next`. A match sets
   `req.Zone` (a `*zone.Zone`).
4. `queryAccess` / `queryACL`: REFUSED with `StatusRefusedACL`.
5. AXFR/IXFR: NOTIMP. The capability slot is defined:
   `TransferProvider.Transfer(zone, serial) (<-chan []dns.RR, error)`
   returns `ErrTransferNotImplemented`. AXFR out is M2.
6. Forward zones set `req.UpstreamGroup = spec.forward.upstreamGroup`
   (default `"default"`) and call `next`.
7. Primary zones are answered from `Zone.Lookup`. AA is set except on
   referrals, NXDOMAIN and NODATA carry the SOA, a loop gives SERVFAIL
   and an OPT record is echoed (1232 B, DO copied). ANY over UDP gets one
   RRset (RFC 8482); over TCP it gets everything.
8. A CNAME/DNAME chain that leaves the zone is chased into other local
   primary zones the client may query, without recursion. If it leaves
   every local zone and RD is set, `next` is called for the target, with
   the forward-zone group hint when the target is in a forward zone. The
   result is spliced after the local chain, takes the target's rcode and
   status, and has AA clear. Without RD, the partial chain is the answer.

Authoritative answers never reach blocking, cache or upstream (§7.1 step
6). `Response.Upstream` is `zone:<name>`.

### `pipeline.Request.UpstreamGroup`

This is a new field: the upstream group an earlier stage chose. The
authoritative stage sets it for forward zones and the upstream stage
forwards to it when non-empty. It is documented on the field.
`stages/upstream` was not edited, so it **does not read the field yet**
(see follow-ups).

## Wiring (`cmd/kudurru serve`)

```go
holder := new(zone.Holder)               // published by zonecache
auth := authoritative.New(holder)        // *authoritative.Stage; RecursionAvailable defaults to true
chain := pipeline.MustNew(ingressStage, auth, upstreamStage)
```

The interface the zone cache must satisfy, as read by the stage:

```go
// internal/stages/authoritative
type Source interface {
    Zones() zone.Set // nil before the first load = unsynced; must not block
}
// internal/zone
type Set interface {
    Synced() bool                 // every watch synced once, or restored from a fresh local snapshot
    Match(qname string) *Zone     // longest-suffix zone, root "." allowed, nil if none
}
```

`zone.Holder` already implements `Source` over an
`atomic.Pointer[zone.Snapshot]`, so the zone cache can embed one. On each
store event it rebuilds the affected zone:

```go
z, errs := zone.Build(dnsZone, recordSets /* creationTimestamp, then ns/name */,
    zone.BuildOptions{Now: time.Now(), MergeUnion: mergeUnion})
// z == nil only when the DNSZone itself is invalid (errs.Zone)
holder.Store(holder.Load().With(gen, z))     // or zone.NewSnapshot(gen, synced, zones...)
holder.Store(holder.Load().WithSynced(gen, true)) // once every watch has synced
// schedule a rebuild at z.NextExpiry() when non-zero
```

The leader-side controller uses the following calls:

```go
serial, changed := zone.StampSerial(dz.Status.Serial, dz.Status.ContentHash, z.ContentHash(), time.Now())
// if changed: UpdateStatus{Serial: serial, ContentHash: z.ContentHash(), RecordCount: z.RecordCount()}
// errs.Zone -> DNSZone condition; errs.RecordSets["ns/name"] -> that DNSRecordSet's
// conditions (the Duplicated detail starts with v1alpha1.ReasonDuplicated)
```

The import API calls `zone.ImportBIND(r, origin)`, then
`im.Objects(name, ns, prefix)` and stamps `im.Serial` via `StampSerial`.
Export is `zone.ExportBIND(w, snapshot.Zone(name))`.

## Measurements

Measured on an 8-CPU shared sandbox (GOMAXPROCS 6, five other agents
running) with Go 1.27.2, `go test -bench . -count 3`, using medians. The
benchmark zone has 100,000 records under `bench.example.`:

- 80k hosts with one A record each, spread over 100 subdomains
- 10k AAAA records
- 5k CNAMEs pointing at hosts
- 3k TXT, 1k SRV and 1k wildcard A records

Qnames are drawn at random. p50/p99 come from timing each call and
include about 25 ns of `time.Now` overhead.

| Benchmark | ns/op | p50 | p99 | allocs/op |
|---|---|---|---|---|
| Lookup100k/hit (A) | 428 | 349 ns | 695 ns | 1 |
| Lookup100k/cname (1 hop) | 716 | 629 ns | 1171 ns | 2 |
| Lookup100k/wildcard | 505 | 401 ns | 1111 ns | 3 |
| Lookup100k/nodata | 360 | 291 ns | 523 ns | 0 |
| Lookup100k/nxdomain | 287 | 222 ns | 347 ns | 0 |
| Build100k (parse + tree + hash) | 613 ms | | | 4.03 M (40/record) |
| Memory retained by a built zone | 318.5 B/record | | | |
| ExportBIND100k | 111 ms | | | |
| SnapshotMatch (6 zones) | 135 | | | 0 |
| authoritative.Serve, authoritative answer | 547 | | | 5 |
| authoritative.Serve, not local (pass-through) | 157 | | | 1 (the test's next) |

Before the exact-match index, the same zone took 725 ns p50 and 1364 ns
p99 for a hit, at 283.6 B/record. Profiles showed the ~17-level pointer
chase through 100k tree nodes dominating. The index costs 35 B/record.
Build time is dominated by `dns.NewRR` parsing.

## Surprises

- The resume branch had been reset to `origin/main`. The WIP commit
  (`1299eec`) was recovered from the branch reflog, fast-forwarded,
  then squashed into the three commits on this branch.
- miekg prints an RFC 3597 record's header as `CLASS1 TYPEnnn`, so
  trimming `Header().String()` off `String()` left the header in the
  rdata of unknown types. The round-trip test caught it. Rdata is now
  everything after the fourth tab of `String()`.
- miekg re-escapes names when printing (`\032` becomes `\ `). The owner
  validator rejected the escaped form, so exported zones with odd names
  did not re-import. Owner names are now canonicalised once
  (`canonicalName`, mirroring miekg's escaping). Unescaped whitespace and
  zone-file syntax are still refused, and so is `$` anywhere in an owner:
  exported at the start of a line it would read as a directive.
- `$GENERATE` expands up to 65,536 records per line, so a few hundred
  bytes of input could ask for millions of records. That is why the
  1,000,000-record import cap exists.
- `strings.Compare(string(b), s)` allocates where `string(b) < s` does
  not. A first attempt to halve tree comparisons went from 1 to 16
  allocations per lookup.
- The test fuzzer for `FuzzImportBIND` stalls for seconds on large
  `$GENERATE` inputs (build plus two exports of 65k records). This is
  expected and not a hang.

## Recommended architecture changes (not applied; for §3.3 / §11.2 / §16)

1. §3.3 `Request`: `Zone *zone.View` should read `Zone any // *zone.Zone`
   (the existing pipeline field), and `UpstreamGroup string` should be
   added. There is no separate `View` type: the snapshot (`zone.Set`,
   `zone.Snapshot`) is the view.
2. Record in §7.1 step 6 / §16: while unsynced, **every** query is
   SERVFAIL, including names under no local zone, because a not-yet-loaded
   zone may cover them. The consequence is no recursion until the zone
   cache syncs (or restores a local snapshot). Positive authoritative data
   is still served.
3. §11.2 status taxonomy: the unsynced SERVFAIL reuses
   `servfail_upstream` with `Upstream = "zonecache:unsynced"`. A dedicated
   `servfail_unsynced` value would read better in the UI. NOTIMP replies
   (NOTIFY, UPDATE, AXFR) use `authoritative`.
4. §4.2: `disabled`, `expiresAt` and `view` are honoured in `zone.Build`
   (called by zonecache). Records with a `view` are withheld until the
   v0.2 rewrite stage can serve them per client group.
5. §3.3 capability list: the transfer capability is
   `authoritative.TransferProvider`. Its signature is unchanged.

## Follow-ups

- `stages/upstream` must read `req.UpstreamGroup` and pick that group
  (owner of the upstream package). Today it ignores the hint and forwards
  forward-zone queries to its single group.
- zonecache: implement `Source`/`Set` (or embed `zone.Holder`), rebuild at
  `NextExpiry`, persist the last snapshot to `local.db` for
  restart-while-store-down (§4.4).
- M2: AXFR out through `TransferProvider` and `transferACL` (reuse
  `zone.ParseACL`), NOTIFY out, TSIG.
- DS at a delegation point where parent and child are both local zones:
  longest-suffix routing sends the DS query to the child (NODATA).
  Answering it from the parent zone needs a DS special case in routing
  (DNSSEC is v1.0, so this is low priority).
- Data below a DNAME owner is accepted. Lookup occludes it correctly, but
  the builder could reject it with a condition (RFC 6672 §2.4). CNAME
  and DNAME at the same owner are already rejected by the CNAME
  exclusivity check.
- A `make bench` gate entry for `BenchmarkLookup100k` (p99 ceiling) once
  `bench/baseline.txt` is re-recorded. That file belongs to the blocking
  package's gate and was not touched here.
