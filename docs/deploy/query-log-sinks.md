# Additional query-log sinks

`querylog.sink` selects where a node records its queries: the local
database (`sqlite`, which the dashboard reads) or log lines (`slog`).
`querylog.sinks` adds further destinations that receive the same recorded
queries next to it. It is a cluster setting, available through the settings
API, the CLI, the settings file and, in Kubernetes, `config.settings`.
Changes apply without a restart.

```yaml
querylog:
  sink: sqlite            # the dashboard keeps its database
  sinks:
    - name: log-lines     # labels this sink in metrics and log messages
      type: slog          # one structured log line per query
    - name: warehouse
      type: clickhouse    # rows in a ClickHouse table
      clickhouse:
        url: https://clickhouse.example:8443
        username: kudurru
        passwordRef: {file: /run/secrets/clickhouse-password}
    - name: logs
      type: loki          # log lines in Grafana Loki
      loki:
        url: https://loki.example:3100
```

Through the API, send a JSON merge patch to `PATCH /api/v1/settings`. The
list is replaced as a whole:

```json
{"querylog":{"sinks":[{"name":"log-lines","type":"slog"}]}}
```

## What a sink receives

Each sink receives every query the node records, after the privacy mode and
the exclusions have been applied. A sink never sees more than the local log
keeps:

- `querylog.privacy` hides clients, names or both before the query reaches
  any sink; under `nothing` no per-query event is sent to an additional sink.
- Names and clients in `querylog.exclude.*` are never sent.
- `querylog.enabled: false` stops every sink.

The one exception is a `dnstap` sink with `raw: true`, which an administrator
sets on purpose: it receives the queries as they were, before the privacy
mode (see [dnstap](#dnstap)). The exclusions still apply to it.

A new sink starts with the queries answered after it was added. Earlier
queries are not sent to it.

## Types

| Type | Destination | Notes |
|---|---|---|
| `slog` | the server's log output, one structured line per query with a `sink` attribute | at most one, and not together with `querylog.sink: slog`, which would write every query twice |
| `clickhouse` | a ClickHouse table, one row per query, over the HTTP interface | needs the `clickhouse` object; see [ClickHouse](#clickhouse) |
| `loki` | Grafana Loki, one JSON log line per query | needs the `loki` object; see [Loki](#loki) |
| `dnstap` | a dnstap collector (DNS-collector, Vector, the `dnstap` command), dnstap messages in Frame Streams over a Unix socket, TCP or TLS | needs the `dnstap` object; see [dnstap](#dnstap) |

The server refuses a type it cannot serve, and settings that belong to
another type. Query lines of `slog` are written at the `info` level, like
those of `querylog.sink: slog`, so they need `log.level` `info` or `debug`.

`clickhouse` and `loki` send their batches with HTTP `POST` requests from the
server itself; no agent or collector runs beside it.

## What one query looks like

`clickhouse` and `loki` export the same record: one JSON object per query.
It is the ClickHouse row and the Loki log line. Fields marked optional are
left out when they have no value.

| Field | Type | Meaning |
|---|---|---|
| `ts` | string | time the query was answered, RFC 3339 in UTC with microseconds, e.g. `2026-11-10T12:34:56.789012Z` |
| `id` | number | the query's ID in the node's query log; with `node` it identifies the query |
| `node` | string | name of the node that answered |
| `proto` | string | `udp`, `tcp`, `dot`, `doh`, `doq` or `doh3` |
| `client` | string, optional | client address, as the privacy mode keeps it |
| `client_name` | string, optional | client name, or the daily pseudonym |
| `client_group` | string, optional | client group the query was attributed to |
| `domain` | string | query name in lower case without the trailing dot; `hidden` when the privacy mode hides names |
| `qtype` | string | `A`, `AAAA`, `HTTPS`, ...; `TYPE65280` for a type without a name |
| `qclass` | number | query class, 1 for `IN` |
| `status` | string | what the server did: `forwarded`, `cached`, `blocked_list`, `authoritative`, ... (the statuses of the query log) |
| `status_class` | string | `permitted`, `cached`, `blocked` or `error` |
| `rcode` | string | `NOERROR`, `NXDOMAIN`, `SERVFAIL`, ... |
| `reply_type` | string | `ip`, `cname`, `nodata`, `nxdomain`, ... |
| `answers` | number | records in the answer |
| `answer` | string, optional | summary of the first answer records; only under `querylog.privacy: full` |
| `latency_us` | number | time to answer, in microseconds |
| `upstream` | string, optional | upstream server that answered |
| `zone` | string, optional | local or forward zone that matched |
| `list` | string, optional | block list that decided |
| `rule` | string, optional | rule that decided |
| `policy` | string, optional | deciding policy, `namespace/name` |
| `cname` | string, optional | name in the CNAME chain that caused a block |
| `dnssec` | string | `secure`, `insecure`, `bogus`, `indeterminate` or `unknown` |
| `ede` | number, optional | extended DNS error code (RFC 8914) |
| `ecs` | string, optional | client subnet the query carried |
| `trace_id` | string, optional | trace ID in hex, when the query was traced |
| `flags` | list of strings, optional | those that were set of `rd`, `do`, `tc`, `ad`, `cd`, `cached`, `stale`, `prefetch`, `ecs`, `traceparent` |

A later version can add fields. It will not rename a field or change its
type.

## ClickHouse

Each batch is one `INSERT INTO <database>.<table> FORMAT JSONEachRow` request
to the HTTP interface (port 8123, or 8443 with TLS).

```yaml
querylog:
  sinks:
    - name: warehouse
      type: clickhouse
      batchSize: 5000                  # larger, less frequent inserts suit ClickHouse
      flushInterval: 5s
      clickhouse:
        url: https://clickhouse.example:8443
        database: kudurru              # default
        table: queries                 # default
        username: kudurru
        passwordRef: {file: /run/secrets/clickhouse-password}
        caRef: {file: /run/secrets/clickhouse-ca.pem}   # only for a private CA
```

| Field | Default | Meaning |
|---|---|---|
| `url` | required | base URL of the HTTP interface, `http` or `https`, without user name, password or query |
| `database` | `kudurru` | database of the table; letters, digits and underscores |
| `table` | `queries` | table the rows go to; letters, digits and underscores |
| `username` | none | ClickHouse user; sent with the password as HTTP basic authentication |
| `passwordRef` | none | where the password is read from, see [Credentials](#credentials) |
| `caRef` | system trust store | CA certificates (PEM) that replace the system trust store for this destination |
| `compression` | `gzip` | `gzip` or `none` |

kudurru does not create the database or the table. Create them once, with a
user that may insert:

```sql
CREATE DATABASE IF NOT EXISTS kudurru;

CREATE TABLE IF NOT EXISTS kudurru.queries
(
    ts           DateTime64(6, 'UTC'),
    id           UInt64,
    node         LowCardinality(String),
    proto        LowCardinality(String),
    client       String,
    client_name  String,
    client_group LowCardinality(String),
    domain       String,
    qtype        LowCardinality(String),
    qclass       UInt16,
    status       LowCardinality(String),
    status_class LowCardinality(String),
    rcode        LowCardinality(String),
    reply_type   LowCardinality(String),
    answers      UInt16,
    answer       String,
    latency_us   UInt64,
    upstream     LowCardinality(String),
    zone         LowCardinality(String),
    list         LowCardinality(String),
    rule         String,
    policy       LowCardinality(String),
    cname        String,
    dnssec       LowCardinality(String),
    ede          Nullable(UInt16),
    ecs          String,
    trace_id     String,
    flags        Array(LowCardinality(String))
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (node, ts, id)
TTL toDateTime(ts) + INTERVAL 90 DAY
SETTINGS non_replicated_deduplication_window = 1000;

CREATE USER IF NOT EXISTS kudurru IDENTIFIED BY '<password>';
GRANT INSERT ON kudurru.queries TO kudurru;
```

Notes on the table:

- The column names and types above are what the rows fit. You can leave
  columns out, reorder them, add your own with defaults, and choose another
  engine, partitioning, sort key and retention (`TTL`). A field the table
  has no column for is skipped, and a column the row has no field for takes
  its default, so the table keeps working when a later kudurru adds a field.
- The requests set two ClickHouse input settings: `date_time_input_format=best_effort`
  (reads the `ts` format into any `DateTime` or `DateTime64` column) and
  `input_format_skip_unknown_fields=1`. The user must be allowed to set
  them; a settings constraint in the user's profile can forbid it, and
  ClickHouse then refuses every batch. They also carry `wait_end_of_query=1`,
  which makes ClickHouse answer only when the insert has finished, so that
  the HTTP status says whether the batch was stored.
- `client` is a `String` because the privacy modes can truncate it, replace
  it with `0.0.0.0` or leave it out.
- `non_replicated_deduplication_window` makes ClickHouse ignore a batch that
  arrives a second time unchanged, which is what a repeated attempt sends
  (see [When a destination is slow or down](#when-a-destination-is-slow-or-down)).
  Replicated tables do this without the setting.
- ClickHouse prefers few large inserts. Raise `batchSize` and
  `flushInterval` as in the example, or enable asynchronous inserts for the
  user (`ALTER USER kudurru SETTINGS async_insert = 1, wait_for_async_insert = 1`),
  when several nodes insert every second.

When ClickHouse refuses a batch, the server log names the HTTP status and
ClickHouse's error code, for example `ClickHouse answered HTTP 404
(ClickHouse exception code 60)`: 60 is an unknown table, 81 an unknown
database, 516 a wrong user name or password, 497 a user without the right
to insert into the table. The message of the error is not logged, because
it can quote the rows.

## Loki

Each batch is one `POST <url>/loki/api/v1/push` in Loki's JSON format.

```yaml
querylog:
  sinks:
    - name: logs
      type: loki
      loki:
        url: https://loki.example:3100
        tenant: home                   # X-Scope-OrgID; omit for a single-tenant Loki
        bearerTokenRef: {file: /run/secrets/loki-token}
        labels: {job: kudurru, site: home}
```

| Field | Default | Meaning |
|---|---|---|
| `url` | required | base URL of Loki or its gateway, `http` or `https`, without user name, password or query; `/loki/api/v1/push` is added |
| `tenant` | none | tenant ID, sent as `X-Scope-OrgID` |
| `username`, `passwordRef` | none | HTTP basic authentication, e.g. the instance ID and an access token of Grafana Cloud |
| `bearerTokenRef` | none | a bearer token, instead of `username` and `passwordRef` |
| `caRef` | system trust store | CA certificates (PEM) that replace the system trust store for this destination |
| `labels` | `job: kudurru` | fixed labels on every stream, at most 16 |
| `structuredMetadata` | `true` | attach the per-query fields below as structured metadata |
| `compression` | `gzip` | `gzip` or `none` |

How a query maps to Loki:

| Part of the entry | Content |
|---|---|
| Stream labels | the fixed `labels`, plus `node` (the node's name) and `status_class` (`permitted`, `cached`, `blocked` or `error`) |
| Timestamp | the time the query was answered, in nanoseconds |
| Log line | the JSON record described in [What one query looks like](#what-one-query-looks-like) |
| Structured metadata | `client`, `client_name`, `domain`, `qtype`, `status` and `trace_id`, each only when it has a value |

The labels have few values on purpose: one node writes at most four streams
(one per status class), however many clients and domains it sees. Do not add
a label whose value differs per query. The values that do differ per query
are structured metadata, which Loki stores next to the line without making a
stream of each value, and which queries can filter without parsing the line:

```logql
{job="kudurru", status_class="blocked"} | domain="ads.example.com"
{job="kudurru"} | client="192.168.1.20" | json | latency_us > 100000
```

Structured metadata needs a Loki that allows it: version 3.0 or newer does by
default (`allow_structured_metadata`, with schema v13). A Loki that does not
accept it answers 400 to every push; set `structuredMetadata: false`, and
filter with `| json` instead: the same fields are in the line.

The privacy mode applies to the labels and metadata as to everything else:
under `hide_domains` the `domain` is `hidden`, and a hidden client is
`0.0.0.0`.

## dnstap

A `dnstap` sink sends [dnstap](https://dnstap.info) messages to a collector
in Frame Streams, with the bidirectional handshake (READY, ACCEPT, START,
then the data frames; STOP and FINISH when the sink closes). The collector
listens; kudurru connects and reconnects after a failure.

```yaml
querylog:
  sinks:
    - name: tap
      type: dnstap
      dnstap:
        target: tls:collector.example:6000   # or unix:/run/dnstap/dnstap.sock, tcp:collector.example:6000
        caRef: {file: /run/secrets/collector-ca.pem}
        # certRef and keyRef: a client certificate for mutual TLS
        types: [client_query, client_response, auth_query, auth_response]
```

| Field | Default | Meaning |
|---|---|---|
| `target` | required | `unix:/path`, `tcp:host:port` or `tls:host:port`; over `tcp` the queries cross the network unencrypted |
| `serverName` | host of `target` | name the collector's certificate must carry (`tls:` only) |
| `caRef`, `certRef`, `keyRef` | system trust store, no client certificate | PEM references, read like the [credentials](#credentials) of the other sinks (`tls:` only) |
| `identity` | `node.name` | dnstap identity of every message |
| `types` | `client_query`, `client_response`, `auth_query`, `auth_response` | which messages are sent |
| `raw` | `false` | send the DNS messages as they were; see below |

Every message carries the version `kudurru/<version>` and, in `extra`,
`status=<status> list=<list> upstream=<group/server> client=<client name>
trace=<id>` (fields without a value are left out). A query a local zone or
local record answered is sent as `AUTH_QUERY` and `AUTH_RESPONSE` with the
zone; every other answer as `CLIENT_QUERY` and `CLIENT_RESPONSE`.
`forwarder_query` and `forwarder_response` add one message pair per query
that went to an upstream. kudurru does not record its upstream exchanges, so
those carry the question and the response code, the client's receive and
answer times and the upstream's name in `extra`, but no addresses.

**Without raw** (the default) a message holds what the local query log
keeps. It is rebuilt from the logged query, after `querylog.privacy`: the
question and the response code with the header flags, never the answer
records; the name `hidden` under `hide_domains` and `hide_domains_clients`;
the client as logged (a /24 or /48 prefix under `anonymize_client`, no
address with pseudonyms, `0.0.0.0` under `hide_domains_clients`) and no
client port. Under `nothing` nothing is sent.

**With `raw: true`** the sink sends the query as the server received it and
the answer as the server produced it (before the transport truncated or
padded it), with the real client address and port, whatever
`querylog.privacy` says, also under `nothing`. Use it to give a collector
that you control everything a DNS server can see. The contract:

- Excluded clients and domains (`querylog.exclude`) are never sent, raw or
  not, and neither are the health name and the dashboard's built-in dig.
- Only an administrator can save it: a change of `querylog.sinks` that keeps
  a raw sink needs the permission `querylog.raw`, which only the `admin`
  role holds. So does a change of `querylog.exclude` while a raw sink is in
  place, because clearing an exclusion widens what the raw sink sends. A refused change is audited, an accepted one is audited as
  `querylog.raw` with the raw sinks before and after. Turning `raw` off, or
  removing the sink, needs only `settings.write`.
- Each node records the audit event `querylog.raw.export` before such a sink
  sends its first message, also when the setting came from the settings
  file or `config.settings`. A node that cannot record the event does not
  open the sink and tries again.
- Only while a raw sink is configured does the server keep the DNS messages
  of each query; without one it packs and copies nothing extra.

A collector that stops reading stalls only this sink: each write gives up
after the sink's `timeout`, a batch that was sent in part is dropped and
counted, the queue overflows into the drop counter, and the sink connects
again when the collector reads. Nothing is ever sent twice.

## dnstap listen

The other direction: `listeners.dnstap` lets a front end such as dnsdist or
Unbound send dnstap to kudurru, so that the queries it answers itself, for
example from its packet cache, appear in this node's query log next to the
ones kudurru answered. It is a host setting (the settings file or the
environment, not the API) and takes effect at restart.

```yaml
listeners:
  dnstap:
    - address: tcp:10.0.0.11:6000       # or unix:/run/kudurru/dnstap.sock, tls:0.0.0.0:6000
      allowFrom: [10.0.0.21, 10.0.0.22] # required for tcp:
      # identities: [edge-1, edge-2]    # only these senders; empty accepts any
      unforwardedIdentities: [edge-1, edge-2] # managed edges: only their own answers
      # ignoreClients: [10.0.0.11]      # e.g. kudurru's own address, see below
      # maxConnections: 16
```

| Field | Default | Meaning |
|---|---|---|
| `address` | required | `unix:/path` (created with mode 0660: put the sender in the group of the kudurru process), `tcp:ip:port` or `tls:ip:port` |
| `tls.certFile`, `tls.keyFile` | none | the listener's certificate (`tls:` only, required there); read at startup |
| `clientCAFile` | none | accept only senders with a client certificate from this CA (`tls:` only) |
| `allowFrom` | none | addresses or CIDRs that may connect; required for `tcp:`, and for `tls:` without `clientCAFile` |
| `identities` | any | only messages with one of these dnstap identities are recorded |
| `unforwardedIdentities` | none | senders that report only the answers they give themselves, never a query they forwarded to kudurru (a managed edge: its `edge.dnstapIdentity`, default its server ID); their messages skip the duplicate check below. With `identities` set, each must be listed there too |
| `ignoreClients` | none | messages about queries from these addresses or CIDRs are not recorded |
| `maxConnections` | 16 | senders served at a time (at most 256) |

The listener accepts the bidirectional handshake (READY, ACCEPT, START, ...,
STOP, FINISH) and unidirectional senders (START first). What becomes a row:

- `CLIENT_QUERY`/`CLIENT_RESPONSE` and `AUTH_QUERY`/`AUTH_RESPONSE`. A query
  and its response (same sender identity, client address and port, DNS ID,
  name and type) become one row. A response alone is one row; a query whose
  response does not arrive within 6 seconds is one row without an answer
  (reply type `unknown`). Such a row has the status `unknown`, because
  kudurru did not make the decision, the client and protocol of the message,
  the answer of the response and its latency.
- Every other type (`FORWARDER_*`, `RESOLVER_*`, `STUB_*`, ...) describes
  the sender's own upstream query for a client query that its `CLIENT_*`
  message already describes. It is counted and not recorded, so one client
  query stays one row.
- **Queries this node answered itself are not recorded twice.** When a front
  end forwards a query to kudurru and also logs it (dnsdist in front of
  kudurru with a rule that logs every response), kudurru has already logged
  it. Every row from dnstap waits one second and is dropped when this node
  answered a query with the same client address and port, name, type and
  class within the message's time window (plus or minus 2 seconds). This
  needs the real client address and port at kudurru, which the PROXY
  protocol of a managed edge provides; the DNS ID cannot be used, because
  dnsdist gives every forwarded query an ID of its own. One answered query
  cancels one message. A query waits 6 seconds for its response, longer
  than kudurru's own 5-second bound on answering, so a query kudurru
  answers slowly (an upstream timeout) is still recognized.
- **List a managed edge in `unforwardedIdentities`.** It logs only packet
  cache hits and its own answers, so the cache miss it forwarded a moment
  before is never claimed by a message of its own. Without the setting, a
  cache hit of the same name and type from the same client address and
  source port within about 2 seconds (a reused TCP, DoT or DoH connection,
  a forwarder with a fixed query port, a retransmit) is taken for that
  miss and dropped. A front end that also logs the responses it relays
  from kudurru (an operator rule, Unbound) must not be listed: give such a
  rule an identity of its own, so that its messages are still checked.
- A message time more than 5 minutes from this node's clock (a sender with
  a wrong clock, or a backlog after a long outage) is replaced by the
  receive time, keeping the latency, and counted in
  `kudurru_querylog_dnstap_clock_skew_total{listener}`.
- Messages with this node's identity (`node.name`) or the identity of one of
  its dnstap sinks are dropped: a sink pointed at its own node's listener
  never records anything twice.
- If the sender is a resolver that kudurru forwards to (kudurru in front of
  Unbound), its `CLIENT_*` messages describe kudurru's own upstream queries:
  list kudurru's address in `ignoreClients`.

Rows from dnstap pass `querylog.privacy` and `querylog.exclude` like any
other query, reach the local database, the live view and every additional
sink, **except dnstap sinks**: a dnstap sink never sends a query it received
from dnstap, so two nodes whose sinks and listeners point at each other
cannot loop. The sender identity is kept in memory with the event and is not
stored; the row's node is this node.

Bounds: a data frame above 192 KiB is skipped (a frame that announces more
than 16 MiB ends the connection), a message that does not decode is
skipped, at most 10,000 messages wait in each stage of a listener (decoded
and not yet taken, queries waiting for their response, rows waiting for
the duplicate check), and further senders
beyond `maxConnections` are disconnected. The listener reads every
connection as fast as the sender writes and never waits for the query log;
what does not fit is dropped. Everything is counted in
`kudurru_querylog_dnstap_frames_total{listener,result}` (`recorded`,
`paired`, `duplicate`, `self`, `identity`, `ignored_client`, `type`,
`malformed`, `oversized`, `overflow`, `dropped`) and
`kudurru_querylog_dnstap_connections_total{listener,result}` (`accepted`,
`refused_peer`, `refused_limit`, `handshake_failed`, `broken`,
`accept_failed`). A failed accept (too many open files, ...) is logged and
retried after a wait that grows from 5 ms to 1 s; it never stops the
listener, and nothing a listener does stops the node or its DNS.

### A managed dnsdist edge

`kudurru edge run --dnstap-target tcp:10.0.0.11:6000` (settings
`edge.dnstapTarget` and `edge.dnstapIdentity`, default the server ID) makes
the supervised dnsdist log, as `CLIENT_RESPONSE`, every answer it gives
without a backend: packet cache hits (`cache_hit_response_rules`) and its
own answers such as refused transfers (`self_answered_response_rules`). The
queries it forwards are logged by the kudurru node that answers them, so
each client query is one row. dnsdist's Frame Streams loggers support
`tcp:` and `unix:` only; keep the TCP path on a trusted network and restrict
`allowFrom` to the edges. List each edge's identity in the listener's
`unforwardedIdentities` (see above), so that a cache hit is never taken for
the query the edge forwarded before it.

## Credentials

A sink never holds a password or token in its settings. `passwordRef`,
`bearerTokenRef` and `caRef` say where the server reads the value. Set
exactly one form:

| Form | Reads | Works when |
|---|---|---|
| `{file: /run/secrets/name}` | a file that holds only the value (a trailing newline is ignored) | the node's own configuration lists the exact path in `credentials.allowedFiles` |
| `{env: NAME}` | an environment variable of the server | the node's own configuration lists the name in `credentials.allowedEnvironment` |
| `{secretRef: {name: my-secret, key: password}}` | one key of a Secret in the namespace kudurru runs in | Kubernetes (`store.driver: kube`), and the Secret carries the label `kudurru.io/query-log-sink: "true"` |

`credentials.allowedFiles` and `credentials.allowedEnvironment` are set on
each host (`kudurru.yaml`, or `config.node` in the Helm chart), never through
the API. Being allowed to change the settings is therefore not enough to
make a server read a file and send it somewhere:

```yaml
# kudurru.yaml on every node
credentials:
  allowedFiles:
    - /run/secrets/clickhouse-password
    - /run/secrets/loki-token
```

On Kubernetes the chart's ServiceAccount can read the Secrets in its own
namespace, and kudurru keeps its own keys there. A sink therefore uses a
Secret only when the Secret says so, with a label that whoever manages the
namespace sets:

```sh
kubectl -n kudurru create secret generic query-log-export \
  --from-literal=clickhouse-password='<password>'
kubectl -n kudurru label secret query-log-export kudurru.io/query-log-sink=true
```

```yaml
# values.yaml
config:
  settings:
    querylog:
      sinks:
        - name: warehouse
          type: clickhouse
          clickhouse:
            url: https://clickhouse.example:8443
            username: kudurru
            passwordRef: {secretRef: {name: query-log-export, key: clickhouse-password}}
```

A Secret without the label, or in another namespace, is never read for a
sink, whatever the settings name.

A grant is not bound to a destination. Whoever may change the settings
(`settings.write`) can name any granted file, any granted variable or any
labeled Secret in a sink and have its value sent, as a password or bearer
token, to a URL of their choice, plain `http` included. Grant and label only
credentials made for these destinations, and not, for example, the token of
a DNS provider integration.

A referenced value holds at most 64 KiB. That is ample for a password, a
token or a private CA, but not for a combined system bundle such as
`/etc/ssl/certs/ca-certificates.crt`: point `caRef` at a file with only the
CA certificates that sign the destination's certificate. A larger value is
refused with `credential is unavailable: it holds more than 64 KiB`.

A sink reads its credential when it starts, and again when the destination
answers 401 or 403. To rotate a credential, change the file or the Secret:
the next batch that is refused makes the sink read the new value and send
the batch again. No restart or settings change is needed. The CA
certificates of `caRef` are read only when the sink starts; after replacing
them, change the sink's entry (any field) or restart the server.

A sink whose reference cannot be read (a path that is not allowed, a Secret
without the label, a missing Secret or key, a value over 64 KiB) does not
start. It keeps trying,
`kudurru_sink_errors_total` counts the attempts, the server log says
`credential reference is not allowed` or `credential is unavailable`, and
the queries that overflow its queue are counted as dropped. DNS and the
local query log are not affected.

Use `https` for a destination on another host. Over `http` the credential
and the queries cross the network in the clear; the server logs a warning
when a sink sends a credential over `http` to a host other than the local
one. Requests go through the proxy named by `HTTPS_PROXY`, `HTTP_PROXY` and
`NO_PROXY`, when the server's environment sets them. A redirect is not
followed.

## Limits of one sink

Every entry has its own queue and its own limits. Omit a field for its
default.

| Field | Default | Range | Meaning |
|---|---|---|---|
| `name` | required | lowercase letters, digits and hyphens, at most 32 characters | unique in the list; `sqlite` and `slog` are taken by `querylog.sink` |
| `type` | required | see above | kind of destination |
| `queueSize` | 10000 | 100 to 100000 | queries that may wait for this destination |
| `batchSize` | 1000 | 1 to 10000, at most `queueSize` | queries per write |
| `flushInterval` | 1s | 100ms to 1m | longest wait before a partial batch is sent |
| `timeout` | 10s | 1s to 2m | opening the destination and each write attempt; with 4s or less, a batch of 64 KiB or more is sent without first asking for `100 Continue` |
| `attempts` | 3 | 1 to 10 | writes of one batch when nothing was sent or the destination answered that it may be sent again; 1 never retries |
| `retryBackoff` | 1s | 100ms to 1m | wait before the second attempt; doubles up to 30s |

At most eight sinks can be configured.

## When a destination is slow or down

A sink cannot slow the server down. DNS answers, the local database and the
dashboard's live view never wait for it. When a destination is slower than
the query rate, stalled or failing:

- its queue fills to `queueSize`, and further queries are dropped for that
  sink only;
- a batch is sent again only when nothing was sent or the destination
  answered that it may be sent again; after `attempts` writes, or after a
  request that got no answer, the batch is dropped. A batch is never moved
  to another sink;
- for `clickhouse` and `loki`, a batch is sent again when no connection
  could be established (nothing was sent), or when the destination answered
  408, 429 or 5xx; a `Retry-After` header is honored up to 30 seconds. A 5xx
  does not always mean that nothing was stored (see below). Any other
  answer drops the batch at once: a 400 or 404 will not get better by
  repeating it. A request that was sent and got no answer before `timeout`
  is an unknown outcome and is dropped too;
- the server logs a warning at the first lost query and at each tenfold
  total, and keeps trying to reach the destination.

Dropped queries stay in the local database (`querylog.sink: sqlite`); only
the copy for that destination is lost.

A repeated attempt can store a batch twice when the destination had stored
it and still answered with an error: a ClickHouse insert that failed part
way, or a 502 or 504 from a proxy in front of the destination that gave up
waiting for it. ClickHouse ignores the repeated batch
when the table deduplicates inserts, as the table above does. Loki usually
drops a line that repeats the stream, timestamp and text of one it holds,
but does not promise to. Loki can also answer 400 for a batch of which it
stored a part (lines it accepted next to lines it found too old); kudurru
counts the whole batch as dropped. Set `attempts: 1` to never send a batch
twice.

## Monitoring

| Metric | Meaning |
|---|---|
| `kudurru_querylog_dropped_total{stage="sink",sink="<name>"}` | queries lost for this sink (queue overflow, an abandoned batch, or a query caught by a change of the sink's entry) |
| `kudurru_sink_sent_total{sink}` | queries the destination accepted |
| `kudurru_sink_errors_total{sink}` | failed attempts to open or write |
| `kudurru_sink_queue_length{sink}` | queries waiting now |
| `kudurru_sink_last_success_timestamp_seconds{sink}` | time of the last successful write |

The shipped `KudurruQuerylogDrops` alert fires on any increase of
`kudurru_querylog_dropped_total`, and the Grafana query-log panel shows the
drops by stage and sink. A queue length that stays near `queueSize` means
the destination cannot keep up: raise `batchSize`, or reduce what is logged
with `querylog.exclude.*`.

The series of a sink disappear when it is removed from the settings.

The same state is in the API: `GET /api/v1/querylog/sinks` (permission
`querylog.read`) lists the sinks the answering node runs, including one that
is still draining after a change, with `queued`, `queue_size`, `sent`,
`dropped`, `errors`, `last_success` and `raw`. Every node runs its own
sinks, so ask each node; the route does not ask the others. It never shows a
credential, an error text or a query.

## Changing and removing sinks

- An entry that is not changed keeps running across unrelated settings
  changes.
- Changing any field of an entry replaces that sink: the old one finishes
  what it holds, the new one starts with new queries. On Kubernetes, a query
  that is being recorded at the very moment of the change can miss both. It
  is counted as dropped for that sink, is not sent to the new one, and stays
  in the local database. This is rare, but the drop alert fires on any
  drop, so changing a sink on a busy server can trigger it once.
- A removed sink gets no new queries. It writes what its queue holds for up
  to five seconds, counts what remains as dropped, and closes once requests
  that began before the change have finished (an open live view can delay
  the close and keep the sink's metrics visible until it ends).
- On shutdown the sinks get up to five seconds to write what they hold; the
  local database does not wait for them. A destination that is stalled or
  unreachable at that moment can add up to five seconds to the shutdown. Its
  sink loses what it still held, and the server names it in the warning
  `additional query-log sink did not drain` with the number of queries that
  were waiting. The warning `query log flush incomplete` is about the local
  query log only. When the local query log itself did not flush in time,
  that warning comes with a single
  `additional query-log sinks stopped because the query log did not flush`
  that lists every sink: they were stopped, not drained, and lose what they
  still held.

How this is built: [the query log pipeline](../architecture/observability.md#additional-sinks).
