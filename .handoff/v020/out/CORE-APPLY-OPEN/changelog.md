### Changed

- On SQLite, a long `kudurru apply` or `kudurru restore` (and a multi-delete in
  the dashboard) now pauses for other callers' reads: when another client's
  read is waiting, the request lets the node publish before its next document,
  so the read is answered instead of failing with `503 generation_pending`. The
  pauses are bounded and make a request at most about twice as slow. Any
  waiting `GET` or `HEAD`, signed in or not, triggers a pause; the bound holds
  either way.

### Fixed

- On SQLite, DNS keeps answering for the node's own zones while a long apply,
  restore or multi-delete commits its documents. Before, a request of 120
  documents could leave existing records of the node's own zones unanswered
  for over a minute. Answers now come from the configuration the node
  published last, and a document is answered once the node has published it.
  The same holds after every API write: right after a write is acknowledged,
  DNS can still answer the previous data (or `NXDOMAIN` for a new name) until
  the node publishes, and a node that cannot publish keeps answering it. Such
  a node reports `prepare_failed` in `kudurru health --ready`.
- DNS answers no longer stop for a few seconds when the node replaces its
  sign-in provider resources or its license state changes. DNS answering does
  not depend on either.
- A SQLite node that cannot publish a new configuration (a `BlockList` URL it
  cannot download, or a zone of an unsupported type) keeps answering for its
  own zones from the configuration it published last. Before, it answered only
  forwarded names.
