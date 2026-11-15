# CORE-APPLY-MULTI changelog lines for `[0.2.0]`

### Fixed

- On SQLite, one run of `kudurru apply` or `kudurru restore` now applies every
  document of its files. Before, a standalone node or the primary of a cluster
  applied only the first new or changed document and answered `unavailable`
  ("current writable serving generation unavailable") for every later one, so
  a file with N new objects needed N runs. The same holds for
  `POST /api/v1/apply`, for `kudurru apply -f a -f b -f c` and for a restore
  into a new node.
- Deleting several objects at once (`POST /api/v1/{kind}:batchDelete`, the
  multi-select delete of the dashboard's object pages) now deletes every
  selected object of one namespace in one request on SQLite. Before, it
  deleted the first one and refused the others. The dashboard now sends the
  requests for a selection across several namespaces one after another, so
  that one of them is no longer refused as a whole.

### Changed

- A request with several documents or names stops when something else is
  changed on the node while it runs: another request that changes data, a
  sign-in, or a write that a follower passes on. Every later item then reports
  `unavailable` with "another change was committed on this node while the
  request ran; this item was not applied; review current state before a new
  decision", and the items before stay applied. Run the command again: it
  reports what is already applied as `unchanged`. The node's own follow-up work
  on the request's objects, such as writing their status, does not stop it.
- While the documents of such a request commit, the node answers no DNS
  query for its own zones and answers other API requests `503
  generation_pending` until it has published again. A request that has run
  for two seconds therefore waits for the node to publish the documents so
  far and pauses for 200 milliseconds before its next document, so local
  answers and other callers wait about two seconds at a time plus the node's
  time to publish instead of for the whole request. Long requests are slower
  as a result. Changes by RFC 2136 updates, the local admin socket and the
  node's own reconciliation do not stop a request.
- A request that begins while the node is still publishing other work can be
  refused as a whole before its first document (`503 generation_pending`, or
  every item `unavailable` with "current writable serving generation
  unavailable"). Nothing was applied; send it again.
- `kudurru apply` and `kudurru restore` now wait up to five minutes for the
  answer by default, as long as the server lets one request run;
  `--request-timeout` still overrides it, and other commands keep 30 seconds.
- An item of such a request that is reached after the request's five-minute
  lifetime ended reports `unavailable` ("the request's lifetime ended before
  this item was reached; it was not applied") instead of "current credential
  is invalid or expired".

This replaces the known limitation that the documentation lane listed for the
0.2.0 section ("On SQLite, one run of `kudurru apply` or `kudurru restore`
applies only the first document that is new or changed"): remove that line.
The limitation for a follower stays, with corrected wording: through a
follower, a file that does not fit between two of the follower's pulls is cut
with `unavailable` ("the follower's source changed"), and repeating the command
through the follower does not finish it; send such a file, and every restore,
to the primary.
