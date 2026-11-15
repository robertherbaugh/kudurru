### Added

- dnstap output. A `querylog.sinks` entry of `type: dnstap` sends every
  recorded query as dnstap messages in Frame Streams to a collector such as
  DNS-collector, Vector or the `dnstap` command, over a Unix socket
  (`unix:/path`), TCP (`tcp:host:port`) or TLS (`tls:host:port`, with
  `caRef` for a private CA and `certRef`/`keyRef` for mutual TLS). Answers
  from local zones and local records are `AUTH_QUERY`/`AUTH_RESPONSE`, all
  other answers `CLIENT_QUERY`/`CLIENT_RESPONSE`; `types` selects which are
  sent, and `forwarder_query`/`forwarder_response` add a summary message
  pair for queries that went to an upstream. The identity is the node name
  unless set, the version `kudurru/<version>`, and `extra` carries the
  status, list, upstream, client name and trace ID. Like every additional
  sink it has its own bounded queue: a collector that is down or stops
  reading loses only its own messages, which are counted, and the sink
  reconnects on its own.

- By default a dnstap message contains only what the local query log keeps:
  it follows `querylog.privacy`, carries the question and the response code
  but never the answer records, and nothing is sent under `nothing`.
  `raw: true` sends the DNS messages exactly as received and answered, with
  the real client address and port, whatever the privacy mode says. Only an
  administrator can save it (new permission `querylog.raw`, held by the
  `admin` role), the change is audited as `querylog.raw`, and every node
  records the audit event `querylog.raw.export` before it sends the first
  raw message; a node that cannot record it does not send. Excluded clients
  and domains (`querylog.exclude`) are never sent, raw or not, and while a
  raw sink is configured a change of `querylog.exclude` also needs
  `querylog.raw`. The server
  keeps the DNS messages of a query only while a raw sink is configured.

- `querylog.raw` in the permission table.
