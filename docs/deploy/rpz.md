# RPZ feeds

A response policy zone (RPZ) is a DNS zone whose records describe blocking
policy. Threat-intelligence vendors and many resolvers publish their block
data this way. kudurru transfers an RPZ from its primaries with AXFR or IXFR,
compiles it in memory and applies it to recursive queries, on both store
drivers (SQLite and Kubernetes). Every node transfers and keeps its own copy;
there is no leader on the DNS path.

## Add a feed

An RPZ feed is a `BlockList` source with `format: rpz` and an `rpz` block. The
list must be in block mode, and a policy must reference it like any other list:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata:
  name: threats
  namespace: default
spec:
  mode: block
  sources:
    - name: vendor
      format: rpz
      rpz:
        zoneName: rpz.vendor.example.
        primaries: ["192.0.2.53", "[2001:db8::53]:5353"]
        transferProtocol: tcp    # tcp (default) or tls
        tsigKeyRef: vendor-xfr.  # optional; a stored or file TSIG key name
      # exclude: ["ads.example.com.rpz.vendor.example."]  # switch single owners off
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata:
  name: default
  namespace: default
spec:
  blockListRefs: [threats]
```

`primaries` are tried in order (1 to 32, host or `host:port`; port 53 by
default). With `transferProtocol: tls` the primary's certificate must verify
for the host name you give. `tsigKeyRef` names a key from
[stored TSIG keys](tsig-keys.md) or the `tsigKeys` file setting, never the
secret itself. `exclude` takes absolute policy owner names inside the RPZ.
RPZ sources have no editable entries: change the feed at its origin.

A list can mix RPZ and ordinary sources. `spec.sources[].enabled: false`
switches one feed off, and the `kudurru.io/enabled` annotation the whole list.

## Supported triggers and actions

| Trigger | Owner in the RPZ | Matches |
|---|---|---|
| QNAME | `name.example.<origin>`, `*.name.example.<origin>` | The query name and every CNAME target in the answer |
| CLIENT-IP | `<prefix>.<reversed address>.rpz-client-ip.<origin>` | The client address (after edge client identity) |
| RESPONSE-IP | `<prefix>.<reversed address>.rpz-ip.<origin>` | Every A and AAAA address in the answer |

| Action | RPZ data | Answer |
|---|---|---|
| NXDOMAIN | `CNAME .` | NXDOMAIN |
| NODATA | `CNAME *.` | NOERROR with no records |
| PASSTHRU | `CNAME rpz-passthru.` | The real answer; stops RPZ evaluation, ordinary lists still apply |
| DROP | `CNAME rpz-drop.` | No response |
| TCP-only | `CNAME rpz-tcp-only.` | Truncated over UDP, the real answer over TCP |
| Local data | Any other records (A, AAAA, TXT, `CNAME target.`, `CNAME *.target.`) | Those records, for the query type or a CNAME |

QNAME owners use DNS wildcard rules, not adblock suffix rules: `*.name.example`
matches names below `name.example` but not `name.example` itself, an exact owner
beats a wildcard, and a name that exists as an empty node in the RPZ is not
matched by a wildcard above it. Local data TTLs never exceed the time left
before the copy expires.

These are ignored and counted: NSDNAME and NSIP triggers (kudurru forwards and
does not see the resolver's name servers), DNAME actions, unknown `rpz-`
triggers and actions, wildcards that are not the first label, a policy at the
zone apex, and malformed owners (an address trigger that is not a valid prefix,
an invalid CNAME target). The other rules of the feed still apply. The feed
state below reports each ignored reason with its complete count and up to five
example owners.

## Precedence

For a recursive query, kudurru:

1. Applies explicit allow lists and allow-only mode first. An allowed name is
   never checked against RPZ.
2. Checks that every RPZ feed the client's policy selects is available. If one
   is missing, unbound, bound to a rotated TSIG key or expired, the query
   answers SERVFAIL with Extended DNS Error 14 "RPZ `<origin>` unavailable". A
   selected feed is required coverage; it is never skipped.
3. Resolves the original query once. A resolver failure is returned as it is.
4. Evaluates the feeds in policy order (the policy's `blockListRefs`, then each
   list's sources). Within one feed CLIENT-IP comes first, then QNAME (query
   name, then CNAME targets), then RESPONSE-IP (longest prefix). The first feed
   with a match decides, so an earlier feed's RESPONSE-IP rule beats a later
   feed's QNAME rule.
5. Without a match, or after PASSTHRU, applies the ordinary block lists to the
   real answer.

A hit is answered with status `blocked_rpz` in the query log, the list
(`namespace/name`) and the rule (`<owner> <action>`). `kudurru query` and
the dashboard's dig page show the same fields.

## Refresh, expiry and restarts

Each node follows the RPZ's SOA timers: it checks the serial every `refresh`
seconds, retries every `retry` seconds after a failure, and stops using its copy
`expire` seconds after the last successful check. An accepted copy is kept in
the node's `local.db`, so a restart reuses an unexpired copy without contacting
the primary. A restart never renews the expiry. On Kubernetes, a pod that
restarts while the apiserver is unreachable reuses only unsigned feeds; see
[runbooks](runbooks.md).

## Check a feed

Feed state is per node: each node reports its own copy. Query a node directly
to compare nodes.

```console
$ kudurru lists rpz -A
NAMESPACE   NAME      SOURCE   ORIGIN                STATE     SERIAL       RULES   IGNORED   EXPIRES   NODE
default     threats   vendor   rpz.vendor.example.   serving   2026101107   1197    4         in 110m   node-a
$ kudurru lists rpz threats -o wide   # primary, confirmation, next attempt, ignored reasons, message
$ kudurru lists rpz threats -o json   # every field
```

`STATE` is one of:

| State | Meaning | DNS for clients whose policy selects the list |
|---|---|---|
| `pending` | No copy yet and no failure | SERVFAIL until the first complete transfer (the list is in the policy, the feed is not) |
| `serving` | An unexpired copy is in use; `lastError` may report a failed refresh | Applied |
| `expired` | The last copy passed its SOA expire | SERVFAIL |
| `unavailable` | No usable copy: a transfer, configuration or credential failure (`reason`, `message`), or an unexpired copy the node cannot use (discarded because the configuration or credential changed while it was accepted, or confirmed after the node's current clock time) | SERVFAIL |

A feed is pending as soon as the node's configuration lists it, and from then on
it is required coverage, so clients whose policy selects it get SERVFAIL until
the first transfer completes. That is different from the short window right
after a start, before the list resolver has compiled the list: until then the
list is not in the policy at all and its feeds are not applied (see
[block lists](../architecture/block-lists.md)). The `/info/messages` diagnosis
for a pending feed is `info`, not an error, because every new feed starts there;
a feed whose first transfer fails moves to `unavailable` (an error).

The same data is available from the API, with `blocklists.read`:

- `GET /api/v1/blocklists/_rpz?namespace=` lists the feeds of every list on
  the answering node.
- `GET /api/v1/blocklists/{namespace}/{name}/rpz` lists one list's feeds
  (404 when the node's configuration has no such list, an empty list when it
  has no enabled RPZ source).

Each item has `node`, `namespace`, `list`, `source`, `origin`, `state`,
`available`, `reason`, `message`, `serial`, `primary`, `lastConfirmedAt`,
`expiresAt`, `nextAttemptAt`, `lastError`, `configurationError`, `counts`
(records, rules, invalid, unsupported, excluded, ignoredRecords,
staleExcludes), `rules` (by trigger: qname, clientIP, responseIP; by action:
nxdomain, nodata, passthru, drop, tcpOnly, localData) and `ignored` (reason,
unsupported, count, examples). This is the node's feed manager at the time of
the request. The dig explanation and policy list counts show what the request's
own serving snapshot applied; both converge within a publication.

`GET /api/v1/info/messages` (and `kudurru doctor`) adds one diagnosis per
feed that is not serving cleanly (source `rpz`: `TransferPending` info,
`RPZTransfer` warning while a copy still serves or error without one,
`RPZExpired` and `ConfigurationUnavailable` errors) and an
`RPZRulesIgnored` info message for a feed with ignored owners.

Prometheus metrics, labeled `list` and `source`:
`kudurru_rpz_feed_available`, `kudurru_rpz_feed_expires_timestamp_seconds`,
`kudurru_rpz_feed_confirmed_timestamp_seconds`, `kudurru_rpz_feed_serial`,
`kudurru_rpz_feed_rules` and `kudurru_rpz_feed_ignored_rules` (also labeled
`reason`). For an alert, compare the expiry with `time()`:

```promql
kudurru_rpz_feed_expires_timestamp_seconds > 0
  and (kudurru_rpz_feed_expires_timestamp_seconds - time()) < 3600
```

## Limits

- A transfer is bounded to 1,000,000 records and 64 MiB.
- The shared dnsdist packet cache caches an answer only while every feed of the
  serving snapshot is available and no feed has a CLIENT-IP rule, and never
  past the earliest feed expiry (SQLite). On Kubernetes the edge cache is not
  flushed when a feed changes, so an edge-cached answer can outlive a feed
  update by its TTL.
- The dashboard does not show RPZ feed state yet; use the CLI, the API or
  the metrics.
