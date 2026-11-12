# Query tracing (OpenTelemetry)

kudurru can record an OpenTelemetry trace of every DNS query and send the
interesting ones to a collector (Jaeger, Grafana Tempo, Honeycomb, an
OpenTelemetry Collector) over OTLP/HTTP. With dnsdist in front, the dnsdist
span and the kudurru spans form one trace. Tracing is off by default. How it
works inside: [architecture/observability.md](../architecture/observability.md#query-tracing).

## Turn it on

Every `tracing.*` key is set on the host (file or environment) and needs a
restart; none can be changed through the API.

```yaml
tracing:
  enabled: true
  endpoint: http://otel-collector:4318/v1/traces   # the path is used as written
  sampleRate: 1000        # traces per million queries: 0.1 % (default)
  slowThreshold: 250ms    # always keep slower queries; 0s turns the rule off
  # acceptTraceparent: true
  # ednsOptionCode: 65500
  # serviceName: kudurru
  # headers: {X-Scope-OrgID: home}
  # bearerTokenRef: {file: /run/secrets/otlp-token}   # also list it in credentials.allowedFiles
  # caRef: {file: /etc/kudurru/otlp-ca.pem}
  # compression: gzip
  # queueSize: 2048
  # timeout: 10s
```

A query's trace is kept and exported when any of these holds:

- it came from a trusted edge peer with a TRACEPARENT option (see below);
- the head sampler picked it (`sampleRate` per million; the decision depends
  only on the trace id, like OpenTelemetry's TraceIdRatioBased sampler);
- it failed: SERVFAIL because the upstreams failed or timed out, DNSSEC
  bogus, an unsynced zone. Refused and dropped queries are policy, not
  failures, and are not kept for that reason;
- it took at least `slowThreshold`.

A kept trace's id appears in the query log (`trace_id` in the Queries API,
the live tail and every query-log sink) and as an exemplar on
`kudurru_dns_request_duration_seconds` (scrape with OpenMetrics, as the
Prometheus `exemplar-storage` feature does). Spans include the question
name only under `querylog.privacy` `full` or `anonymize_client`, and the
client address only under `full`.

## dnsdist in front

dnsdist 2.1 or later can trace queries and pass the trace on in an EDNS
TRACEPARENT option (code 65500 by default):

```lua
setOpenTelemetryTracing(true)
-- trace 0.1 % of queries and pass the trace id to the backends
addAction(ProbaRule(0.001), SetTraceAction(true, {sendDownstreamTraceparent=true}))
```

kudurru removes the option from every query before it does anything else.
It continues the edge's trace only when the query arrived with an
authenticated PROXYv2 header: from a peer in `listeners.proxyProtocolFrom`
with the shared secret of `listeners.proxyProtocolSecretFile` (loopback
without a secret, as for the client address itself). From anyone else the
option is ignored, so clients cannot inject or choose trace ids
(`kudurru_tracing_traceparent_total{result="untrusted"}` counts them). Set
`acceptTraceparent: false` to ignore the option from edges too.

dnsdist sends the option only for the queries it traces, so kudurru keeps
every trace that continues an edge trace: dnsdist 2.1.3 leaves the trace
flags at 00 even for traced queries, and honoring them would drop every
edge trace. Choose the sample on the edge (`ProbaRule`) when dnsdist is in
front.

The kudurru edge supervisor (`kudurru edge`) does not render these dnsdist
lines yet; add them to a hand-written dnsdist configuration.

## Passing the trace on to upstreams

Queries to upstream servers never carry the client's EDNS options. To
continue a trace at a server you run (another dnsdist, PowerDNS Recursor),
flag its group:

```yaml
upstreams:
  groups:
    internal:
      servers: [10.0.0.53]
      tracePropagation: true
```

Each attempt to such a server then carries a TRACEPARENT whose parent is
that attempt's span, for the queries whose trace is sampled (`sampleRate`,
or a trace continued from the edge). The receiver treats the option's
presence as the decision to trace, so kudurru does not send it for the
other queries; a query kept only because it failed or was slow shows its
attempts but does not continue downstream. Never flag a group of public resolvers: they would
learn trace ids. Recursive groups refuse the flag. Another kudurru receiving
the option this way ignores it unless the query came through its own
authenticated PROXYv2 path.

## When the collector is slow or down

Answers never wait for tracing. Kept traces wait in a queue of `queueSize`
traces and are sent in batches every two seconds, one request at a time,
each bounded by `timeout`. When the queue is full further traces are dropped
and counted; a batch the collector refuses or does not answer is dropped and
counted, never sent again. Watch:

| Metric | Meaning |
|---|---|
| `kudurru_tracing_traces_total{result="exported"}` | traces the collector accepted |
| `kudurru_tracing_traces_total{result="queue_full"}` | dropped because the collector could not keep up |
| `kudurru_tracing_traces_total{result="export_failed"}` | in batches the collector refused or did not answer |
| `kudurru_tracing_traces_total{result="not_kept"}` | traced queries neither sampled, failed nor slow |
| `kudurru_tracing_queue_length` | kept traces waiting |

A failed batch also logs `trace export failed; the batch is dropped` with
the reason, never the token.
