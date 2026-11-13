# Managed root anchors (RFC 5011)

Recursive groups validate DNSSEC from the root. By default the root trust
anchors are static: the IANA DS records shipped with the binary, or the DS
records in `upstreams.recursion.trustAnchors`, until you change them. With
`upstreams.recursion.anchorMode: managed`, a SQLite node keeps its own RFC 5011
state instead. It starts from your explicit DS seeds and polls the root DNSKEY
set over TCP. It accepts a new root key only after a 30-day hold-down, and
honors a revocation as soon as it sees one.

Managed mode is opt-in. It applies to recursive groups only; forwarded
validation (`dnssec.*`) always uses static anchors, as described in
[DNSSEC validation](dnssec-validation.md).

## Requirements

- The SQLite store driver. The Kubernetes driver refuses `anchorMode: managed`
  at startup and on a settings change (D370).
- `upstreams.recursion.dnssec: true` and a recursive upstream group.
- Explicit DS seeds in `upstreams.recursion.trustAnchors`, at most 64. Shipped
  anchors are never used as a fallback. **At least one seed must be a key that
  currently signs the root DNSKEY set.** Bootstrap needs it, and the seeds are
  fixed once the first bootstrap succeeds (see below). On November 13, 2026,
  the root DNSKEY set is signed by KSK-2024 (key tag 38696); KSK-2017 (20326)
  is still published but no longer signs. The shipped pair is a safe seed set:

  ```yaml
  upstreams:
    recursion:
      trustAnchors:
        - ". IN DS 20326 8 2 E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D"
        - ". IN DS 38696 8 2 683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16"
  ```

  Verify the DS records against IANA's `root-anchors.xml` through a channel you
  trust before you use them.
- Every node that holds an anchor writer or a follower checker reaches the root
  servers (or your `rootHints`) directly over TCP port 53. The poll sends the
  root DNSKEY question with RD clear and EDNS (1232 bytes, DO set), and
  accepts only a complete answer with no authority records and at most the OPT
  record. All 13 root letters answer this way (see "How it is qualified").
- A host-only `upstreams.recursion.managedTopology` on every node: `direct`
  when clients reach the node's own listeners, `edges` behind dnsdist. It is
  read at startup only.

## Enable it

### Standalone node

1. Set `upstreams.recursion.managedTopology: direct` in the host configuration
   file.
2. Set `anchorMode: managed`, `dnssec: true` and the DS seeds, then restart.

```yaml
upstreams:
  groups:
    default: { mode: recursive }
  recursion:
    managedTopology: direct
    anchorMode: managed
    dnssec: true
    trustAnchors: ["<verified root DS>", "<verified root DS>"]
```

At startup the node makes one TCP acquisition and bootstraps from the seeds.
It does not report ready, and answers SERVFAIL (authoritative data included)
until the bootstrap commits. Managed mode fails closed for the whole node.

### Enrolled SQLite cluster

`anchorMode` is a cluster setting and `managedTopology` is a host setting. A
member without `managedTopology` that receives `anchorMode: managed` refuses
to publish, so set the topology everywhere first:

1. On every member, followers first and the primary last, set
   `managedTopology: direct` (or `edges`, below) in the host configuration and
   restart it. Nothing changes yet: the topology alone activates nothing.
2. On the primary, set `anchorMode: managed`, `dnssec: true` and the seeds
   (for example with `kudurru settings set`), and restart it. Its elected
   updater bootstraps once for the cluster.
3. Followers install the managed history with their scheduled pulls. Each
   one also runs its own deny-only root check: it can retire its own output on
   a revocation it sees, but it never adds trust.

A member that joins later gets the history with its first image
(`cluster join --offline`). Configure its topology before its first start.

### Behind dnsdist edges

Use `managedTopology: edges`, and list every caching edge in
`upstreams.recursion.managedEdges` on each backend. Start the edges with
`managedCacheBypass` and `managedObservationAddress` before you enable
managed mode on the backends. The details are in the
[architecture page](../architecture/upstreams-and-cache.md#automatic-root-anchor-lifecycle-status).

## Monitor it

- `kudurru cluster anchors` (or `GET /cluster/anchors`, permission
  `cluster:read`) on any running node shows the owner (phase, generation,
  every key with its DS key tag, RFC 5011 status, first seen and remaining
  hold-down), the next refresh, quarantine, the node's local denials, and the
  updater and supervisor states.
- `kudurru anchors status --offline --database /var/lib/kudurru/state.db`
  reads the same state from a stopped node.
- Metrics: `kudurru_root_anchor_worker_state`,
  `kudurru_root_anchor_admission_current` and
  `kudurru_root_anchor_adoption_refused`.

Alert on these conditions:

- `owner.quarantined` is true. The node has no trusted key and answers
  SERVFAIL. Follow the [recovery runbook](runbooks.md#recover-a-quarantined-managed-root-anchor).
- The updater is stopped (`updater.state`). A complete but invalid reply, a
  failed authentication or a refused commit ends the updater for that writer
  term, without retries. The node keeps serving its accepted state. Restart
  the node, or wait for the next term, to poll again.
- A key stays `AddPend` for much longer than 30 days. See "Hold-down and
  restarts" below.

## Behavior to know

**Seeds are fixed after the first bootstrap.** The managed history is bound
to the seed set it started from. If you change `trustAnchors` later, or switch
back to `anchorMode: static`, the history is not reset: the updater refuses to
run under the different policy, and retained managed history is never
replaced by static or shipped trust. To start over you need a new database,
or the recovery path after a quarantine.

**A wrong seed refuses the bootstrap.** The seed must match a key that signs
the DNSKEY set and does not carry the REVOKE flag, and the signature must be
current. Otherwise the one complete reply ends the updater for that term: the
node stays unready and answers SERVFAIL. The log line is
`root anchor updater stopped for this writer term`. Fix the configuration,
then restart: each start makes one new attempt.

**Hold-down and restarts.** A new root key becomes trusted at the first
authenticated poll strictly after 30 days of accrued hold-down (or the DNSKEY
TTL, if longer). No extra poll is needed beyond that one, but a poll that
lands exactly on the 30-day mark waits for the next. Only time measured
inside one process counts: the node checkpoints its progress at every
committed poll, and a restart or failover loses the time since the last
checkpoint. It never loses the time already committed.
Polls are as frequent as RFC 5011 section 2.3 allows: half the DNSKEY TTL,
at least one hour. At the real root (TTL two days) that is daily. A node
that restarts more often than once a day therefore accrues no hold-down
credit. If the root then revokes the old key, the node is quarantined. Keep
writers running for days at a time. Before a planned root key rollover, check
that the new key's `holdDownRemainingSeconds` is going down.

**Revocations are permanent.** A key that is published with REVOKE set and
signed by itself is revoked at once and stays a tombstone forever. This
implementation never removes it after RFC 5011's 30-day remove hold-down.
A revoked key never becomes trusted again, even when it is published again
without the flag.

**Missing keys stay trusted.** A trusted key that disappears from the DNSKEY
set stays trusted as `Missing` until it is revoked. If it returns, it is
trusted again without a new hold-down.

**Losing every trusted key quarantines.** For example, the only trusted key
is revoked while its successor is still in hold-down, so the successor loses
its only supporter. The node answers SERVFAIL for validated recursion until
an operator recovers it offline with independently verified DS records
([runbook](runbooks.md#recover-a-quarantined-managed-root-anchor)).
Only the revoked key itself signs that last observation, so the node keeps
its old refresh time, which has already passed. It polls once more at once.
That poll cannot be authenticated, and the updater logs
`root anchor updater stopped for this writer term`. This is expected after a
quarantine and changes nothing.

## How it is qualified

Hold-down uses the production constants (30 days, one-hour minimum refresh),
and release binaries have no clock seam. The timing is proven in layers
(decision D398):

- The pure RFC 5011 state machine runs through a 39-day timeline (hold-down,
  the fresh poll after it, Missing, revocation, a restart in the middle of the
  hold-down, quarantine) on DNSKEY replies that BIND and Knot DNS really
  signed and served (`TestRFC5011RolloverFromBINDWires`,
  `TestRFC5011RolloverFromKnotWires` in `internal/trustanchors`).
- Separate `kudurru serve` processes bootstrap from a root that BIND
  (RSA/SHA-256, ECDSA P-256, Ed25519) or Knot DNS signs, and answer
  AD-validated recursion through a signed child with exactly one acquisition.
  They refuse a wrong DS, a revoked seed and an expired signature, and they
  ignore an unsupported Ed448 key. They also observe a BIND-made revocation
  at the committed refresh (`TestBINDManagedRoot*`,
  `TestKnotManagedRootBootstrap` in `test/interop`). For the revocation, a
  `-tags licensetest` build moves only the updater's due time forward. The
  acquisition and the commit are production code.
- `KUDURRU_LIVE_DNS=1 go test -run TestLiveRootManagedBootstrap ./cmd/kudurru`
  runs the production acquisition and the strict parser against every root
  server address. It bootstraps with each shipped seed and reports which
  seeds authenticate today.

No test drives a key from `AddPend` to `Valid` through a running node in real
time, because that takes 30 days. The pure-state proof above covers that step.
