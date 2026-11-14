### Added

- OpenTelemetry tracing of DNS queries (`tracing.*`, off by default): every stage and every upstream attempt becomes a span, and kept traces are sent to an OTLP/HTTP collector. A trace is kept when it is sampled (`tracing.sampleRate`, 0.1 % by default), when it continues an edge's trace, when resolution failed, or when it took at least `tracing.slowThreshold` (250 ms); traces kept only because they failed or were slow are capped at 100 per second. An unreachable or slow collector never delays answers, and a query whose trace is not kept costs two small allocations.
- dnsdist TRACEPARENT handling: a query that arrives from an authenticated edge (PROXYv2 with the shared secret) continues the edge's trace from its EDNS TRACEPARENT option (code 65500), so dnsdist and kudurru spans form one trace. The option is removed from every query and ignored from any other sender.
- Kept traces link from the query log (`trace_id` in the Queries API, the live tail and every query-log sink) and from the `kudurru_dns_request_duration_seconds` histogram as exemplars.
- `upstreams.groups.<name>.tracePropagation` passes sampled traces on to servers you run (another dnsdist, PowerDNS Recursor); other upstreams never receive the option.
- Metrics `kudurru_tracing_traceparent_total`, `kudurru_tracing_traces_total` (including `tail_limited`) and `kudurru_tracing_queue_length`.
