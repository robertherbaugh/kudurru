### Added

- dnstap listen. `listeners.dnstap` accepts dnstap in Frame Streams from a
  front end such as dnsdist or Unbound over a Unix socket (`unix:/path`),
  TCP (`tcp:ip:port`, only from the addresses in `allowFrom`) or TLS
  (`tls:ip:port`, optionally only with a client certificate from
  `clientCAFile`), and records the client queries the messages describe in
  this node's query log, through the same privacy mode and exclusions as
  every other query. A query and its response are one row; forwarder and
  resolver messages, which describe the sender's own upstream queries, are
  counted but not recorded. `identities` limits the accepted senders and
  `ignoreClients` drops messages about given clients. It is a host setting
  and takes effect at restart.
- `unforwardedIdentities` on a dnstap listener names front ends that report
  only the answers they give themselves, such as a managed edge. Their
  packet cache hits are always recorded, even right after the same client
  port asked the same question and kudurru answered it.
- Each client query is one row: a message about a query this node answered
  itself (a dnsdist in front of kudurru that logs every response) is
  recognized by the client address and port, the question and the time, and
  dropped. A query kudurru received from dnstap is never sent by a dnstap
  sink, and messages with the node's own identity are dropped, so a sink and
  a listener cannot feed each other in a loop.
- The listener never slows down a sender or DNS: it reads every connection
  as fast as it is written, and drops and counts what does not fit, frames
  above 192 KiB and messages that do not decode, in
  `kudurru_querylog_dnstap_frames_total` and
  `kudurru_querylog_dnstap_connections_total`. A failed accept, such as too
  many open files, is retried and never stops the listener or the node. A
  query waits up to 6 seconds for its response, so a query kudurru answers
  slowly is still one row. A message time more than 5 minutes off this
  node's clock is replaced by the receive time and counted in
  `kudurru_querylog_dnstap_clock_skew_total`.
- `kudurru edge run --dnstap-target tcp:ip:port|unix:/path` (settings
  `edge.dnstapTarget` and `edge.dnstapIdentity`) makes the managed dnsdist
  send dnstap of the answers it gives without a backend, from its packet
  cache or its own rules, so that they appear in the query log too. List
  the edge's identity in the listener's `unforwardedIdentities`.
