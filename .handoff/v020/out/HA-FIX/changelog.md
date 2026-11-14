# HA-FIX changelog lines for `[0.2.0]`

### Added

- `kudurru iam rolebindings get` and `GET /api/v1/iam/rolebindings/{namespace}/{name}`
  now work on Kubernetes: the item read returns the RoleBinding object with its
  UID and resource version. RoleBinding writes through the API remain
  SQLite-only.

### Changed

- On Kubernetes, public CLI login (`kudurru login` without `--token` or
  `--username`, and `--device`) is refused with `503 cli_login_unavailable` and
  a hint; the CLI prints a message that names `--token` and `--username`. The
  provider logout continuation routes answer `503 provider_logout_unavailable`.
  Both need the complete SQLite runtime.
- On SQLite, an unsafe request to a path under `/api/v1/zones`,
  `/api/v1/unblockrequests` or a BlockList's source entries that is not a route
  now answers the ordinary `404 not_found` instead of
  `503 current_command_unavailable`. Scripts that matched that key for a
  mistyped path see `not_found`.
- `POST /api/v1/settings/sections/{section}/ownership/{action}` and
  `.../yaml` on SQLite keep answering `503 unavailable` and now carry a hint
  that names `kudurru settings set` and `kudurru settings get`.
- `PATCH /api/v1/node/config` and `PATCH /api/v1/settings` declare the
  `501 read_only` answer that a server without a node configuration writer
  (Kubernetes) already gave for node-scope changes. The OpenAPI document, the
  generated Go client and the dashboard types include it.
- The dashboard's Authenticator panel says "Authenticator management is
  unavailable on this server" on Kubernetes instead of a generic status error.

### Fixed

- On Kubernetes, six API routes that the server forwards to its authentication
  stack answered a plain-text `404 page not found` instead of the JSON error
  envelope: the RoleBinding item read, the three `/auth/cli` routes and the two
  `/auth/oidc/{idp}/logout` routes. Every forwarded `/auth` and `/iam` route now
  answers with a result or the envelope.
- `kudurru apply`, the dashboard's block list, client group and policy editors
  (`POST`, `PUT`, `DELETE` on `/api/v1/blocklists`, `/clientgroups`,
  `/policies`) and `POST /api/v1/{kind}:batchDelete` now work through an
  enrolled SQLite follower. They reached the forwarding path but were refused
  with a misleading "the follower's source changed" error, although `0.2.0`
  already described `kudurru apply` as forwarded.
- A write sent through an enrolled SQLite follower (an object write, a zone
  record command, `POST /api/v1/dns/blocking` or a local login) could answer
  `503 completion_unknown` although the primary had committed it, when another
  write on the primary, such as the status update of an object created a moment
  earlier, landed between the commit and the answer. The primary now answers a
  forwarded command before any other write can proceed. A multi-document
  `kudurru apply` through a follower hit this most often, for the document after
  a newly created one.
- A follower that cannot bind a request for forwarding now says why: the
  follower's data changed, the request's 30-second lifetime ended, the request
  was canceled, or the route is not forwarded. In each case the write was not
  applied. A request whose lifetime ended, or that was canceled, answers
  `503 unavailable` with that message for a single object, or per item for
  `kudurru apply` and batch deletion, instead of `401 unauthenticated`,
  "current credential is invalid or expired".

### Notes for operators

- Through a follower, `kudurru apply` and batch deletion send one command for
  each document or name inside one 30-second request that stays bound to the
  data the follower had when the request began. Items that are not reached
  before the lifetime ends, or that follow the follower's next pull (five
  seconds by default), report `unavailable` and are not applied; earlier items
  stay committed and nothing is replayed. Run the apply again (committed
  documents report `unchanged`), or send an apply of many documents or a large
  restore to the primary.
- A write through a follower whose exchange with the primary fails, including
  an unreachable primary, answers `503 completion_unknown` (per item for
  `kudurru apply` and batch deletion): the follower cannot tell whether the
  primary received it. Inspect the object before a new decision.

### Notes for the integrator (not changelog text)

- The second, third and fourth Fixed bullets above correct behavior that no
  release shipped: follower forwarding is new in `0.2.0`. Fold them into the
  forwarding entry if the changelog should not list fixes to unreleased
  behavior.
- The review asked that the changelog not claim `kudurru apply` as forwarded
  while a two-new-document apply was unreliable. The cause is fixed on the
  primary (see the ledger), so the `kudurru apply` bullet stays; the operator
  notes keep the two documented limits (the lifetime and the follower's next
  pull).
- The review-fix pass also kept the follower's 30-second bound on the unsafe
  requests under `/api/v1/objects/` and `/api/v1/zones/` that are not forwarded
  (`POST /zones/{zone}/notify` is the registered one). That is the behavior
  before this package, so it has no changelog line.
