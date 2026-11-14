### Fixed

- A signed RFC 2136 UPDATE sent through an enrolled SQLite follower during a
  burst is no longer answered SERVFAIL when the follower installs the
  primary's previous commit just before it passes the update on: the follower
  waits for its current publication and sends the update once. An UPDATE
  through a follower still answers SERVFAIL when it could not be sent or
  answered within the four-second forward bound (a slow publication on a
  loaded primary counts here), when the follower cannot reach its primary or
  is not admitted by it, when the primary committed the update but could not
  publish it, or when the zone is retiring. The follower never sends an
  update twice; retrying an idempotent update is safe.
- A repeated delivery of the same forwarded UPDATE is answered with the
  primary's retained answer even when the primary's publication changes at
  that moment, instead of SERVFAIL.
- A follower whose pull was canceled or timed out in the middle of an
  install no longer refuses every later pull with "store/sqlite: anchor
  lifecycle unavailable" until it is restarted.
- A follower's pull right after a commit on the primary is no longer refused
  and logged as "replica: unauthorized peer"; the primary reads the image
  again.
- A connection attempt to the primary that times out is reported as an
  unknown outcome in every case, as documented, instead of sometimes as "not
  applied".
- Shutting down a node with managed root trust anchors no longer logs "root
  anchor updater stopped for this writer term: sql: transaction has already
  been committed or rolled back".
