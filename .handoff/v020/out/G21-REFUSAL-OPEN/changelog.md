### Fixed

- Rollup recovery evidence (`GET /querylog/rollup-evidence` and its `verify`)
  on a node without a central database now answers `503 unavailable` with a
  hint that names `database.dsn` and `database.querylog.ship`, instead of a
  bare "central rollup recovery unavailable".
- A DHCP setup write (`PUT /node/dhcp/v4/setup`) on a Kubernetes release with
  DHCP off now says to set `dhcp.enabled` and `dhcp.serverAddress` in the Helm
  values, as the setup read does. On a SQLite node that is not on Linux it
  says that DHCP runs only on Linux, instead of asking for a setting that
  does not help.
- A node asked for a peer's health now explains a failure: without
  internal credentials or a peer address it names the internal listener
  settings, and when the peer is down, times out, refuses the credential or
  runs a version without the route (`peer health: unsupported`) it says what
  to check, or to read the health on that node itself. History and live-tail
  reads that no requested node answered name the internal credentials, the
  query log on each node, and `scope=node`.
- A change sent to a SQLite node just as its writable state changed (for
  example right after another change) still answers `503 unavailable`
  ("current writable serving generation unavailable"), now with a hint:
  read the current state, then send the request again if it is still
  needed.
- `GET /integrations/manifests` on a server without the integration runtime
  answers with the same hinted refusal as the other integration routes.

### Changed

- The operator page `docs/deploy/capabilities.md` explains both meanings of
  `501 not_implemented`: an integration provider without the command, and a
  server started without the part of the runtime that the message names.
