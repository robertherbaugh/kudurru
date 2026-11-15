# HA-REFUSAL changelog lines for `[0.2.0]`

### Added

- `GET /api/v1/cluster` and `kudurru cluster status` report the replication
  role of the node that answers on the SQLite driver: `role` (`standalone`,
  `primary` or `follower`) and `primary`. `kudurru cluster status` has a new
  `ROLE` column. On an enrolled follower `leader` now names the primary
  instead of the follower itself, and `GET /api/v1/nodes` no longer marks the
  follower as leader.
- An enrolled follower's `GET /api/v1/cluster` carries `primaryRequired: true`
  with `forwardedOperations` and `localOperations`, the operation IDs that the
  follower sends to its primary or runs itself. Every other changing operation
  is refused there, so a client can disable it beforehand. The OpenAPI
  document carries the same classification for every changing operation as
  `x-kudurru-follower`, and the endpoint index of the reference shows it in an
  "On a follower" column.
- `GET /api/v1/auth/status` on an enrolled follower carries `role: follower`
  and the primary's name and lists no OIDC provider, because provider sign-in
  runs on the primary. Local password login keeps working on a follower.
- `POST /api/v1/cache/flush` (`kudurru cache flush`) works on an enrolled
  follower: it flushes the response cache of the node that answers, with the
  usual `cache.flush` permission and CSRF checks, and writes nothing to the
  cluster's data.
- The CLI prints a hint for `primary_required`, `completion_unknown`,
  `cluster_fenced` and `cluster_conflict`.

### Changed

- On an enrolled SQLite follower, every changing request that the follower
  neither forwards to its primary nor runs itself now answers
  `503 primary_required`. The message names the primary, the hint says where
  to send the request, and there is no `Retry-After` header. Before, these
  requests answered `503 unavailable`, "current writable serving generation
  unavailable" (settings, allow and deny lists, identity administration, own
  tokens, password and authenticators, sign-out, unblock decisions, import
  and migration runs, zone NOTIFY), `503 unavailable` with other texts (stored
  TSIG keys, OIDC sign-in) or `409` and `503` answers of the integration
  routes. Scripts that
  matched `unavailable` for these cases on a follower see `primary_required`;
  the status stays `503`. The refusal comes after the credential and
  permission checks, so `401` and `403` are unchanged.
- `POST /api/v1/imports/preview` and
  `POST /api/v1/imports/runs/{id}/decommission/preview`, which write nothing
  and used to run on an enrolled follower, now answer `503 primary_required`
  there with the rest of the import flow: each previews an apply that only the
  primary runs, and the decommission preview's revision must match the
  primary's run. Preview against the primary.
- `kudurru login --username` against an enrolled follower now reads
  `GET /api/v1/auth/status` first and refuses with `primary_required` before
  it sends the password. Before, the forwarded login created a session on the
  primary, the token step then failed, and the session stayed valid until it
  expired.
- `POST /api/v1/blocklists/{namespace}/{name}/refresh` (`kudurru lists
  refresh`) on an enrolled follower answers `503 primary_required` instead of
  `202`. Lists are fetched by the primary, so the earlier `202` had no effect.
- A DHCPScope object written through an enrolled follower (`/objects/dhcpscopes`,
  a document of `kudurru apply`) is refused with `primary_required` instead of
  `503 unavailable`, "this object kind cannot be written through a follower";
  `POST /api/v1/dhcpscopes:batchDelete` is refused as a whole.
- A member whose write authority is fenced or in conflict answers every API
  request, including OIDC browser and CLI provider login, with
  `503 cluster_fenced` or `503 cluster_conflict`, with a hint and without
  `Retry-After`, instead of `503 generation_pending` or `503 unavailable` with
  `Retry-After: 1`. The conflict hint names `kudurru cluster resolution
  --offline` and `kudurru cluster resolve --offline`. `generation_pending`
  with `Retry-After` remains the answer of a node whose next publication is
  still being prepared.
- `GET /api/v1/settings/_schema` marks every setting `readOnly` on an enrolled
  follower, where `PATCH /settings` and `PATCH /node/config` are refused. Set a
  follower's `log.level` and `log.format` in its configuration file or
  environment.
- On an enrolled follower, and on a fenced or conflicted member, the block
  page shows "This server cannot take a request for access right now" in place
  of the unblock request form, and a posted form answers `503`. Only the
  primary stores unblock requests; before, the form was shown and its post
  failed with "The request could not be saved".

### Notes for the integrator (not changelog text)

- `docs/deploy/sqlite-cluster.md` (docs lane, not in this lane's tree) still
  says in its section 4 table that an unforwarded change answers
  `503 unavailable` and, near line 196, that a follower "names itself as the
  leader" in `cluster status`. Both statements are overtaken by this package:
  the answer is `503 primary_required`, and `cluster status` shows
  `ROLE follower` with the primary as `LEADER`.
- Follower forwarding is new in `0.2.0`, so the "Changed" bullets about
  follower answers describe behavior that no release shipped. Fold them into
  the forwarding entry if the changelog should not list changes to unreleased
  behavior. The `cluster_fenced`, `cluster_conflict`, block page and `leader`
  bullets likewise concern the enrolled SQLite path, which is new in `0.2.0`.
