### Added

- Additional query-log sinks. `querylog.sinks` lists up to eight destinations
  that receive every recorded query next to the local database or log lines
  selected by `querylog.sink`, with the privacy mode and exclusions already
  applied. Each sink has its own bounded queue, batch size, flush interval,
  timeout and attempt budget, so a slow, stalled or failing destination loses
  only its own queries and never delays DNS answers, the local database or the
  live view. The first type is `slog`: one structured log line per query while
  the dashboard keeps its database. Changes apply without a restart: an
  unchanged sink keeps running, a new one starts with new queries only, and a
  removed one finishes what it holds. On Kubernetes, a query recorded at the
  very moment a sink's entry is changed can miss both the old and the new
  sink; it is counted as dropped for that sink and stays in the local
  database. See `docs/deploy/query-log-sinks.md`.

- Per-sink query-log metrics: `kudurru_sink_sent_total`,
  `kudurru_sink_errors_total`, `kudurru_sink_queue_length` and
  `kudurru_sink_last_success_timestamp_seconds`, labeled by sink name, and
  per-sink loss in `kudurru_querylog_dropped_total{stage="sink",sink}`. The
  existing `KudurruQuerylogDrops` alert and the Grafana query-log panel cover
  them.

### Changed

- The query-log collector warns in the server log when an additional sink
  loses queries: at the first loss and at each tenfold total, with the sink
  name and the reason.

- Shutdown gives additional query-log sinks up to five seconds to write what
  they hold. A destination that is stalled or unreachable at that moment can
  add up to five seconds to the shutdown; its sink is named in the warning
  `additional query-log sink did not drain`, with the number of queries that
  were still waiting. `query log flush incomplete` continues to mean that the
  local query log itself could not be flushed.
