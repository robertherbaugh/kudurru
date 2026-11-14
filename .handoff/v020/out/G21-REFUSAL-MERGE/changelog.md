### Changed

- `docs/deploy/capabilities.md` lists the API service account, service
  account token and identity provider commands, with the Kubernetes refusal
  `503 iam_management_unavailable` and what to do instead (change the
  `ServiceAccount` and `IdentityProvider` objects with `kubectl`). The local
  user row now covers changing and deleting a user.

### Fixed

- `GET /inventory` on a server started without the client inventory now
  says where it is mounted and names `GET /api/v1/clients`, instead of a bare
  "the client inventory is not mounted on this server".

Note for the integrator: the G21-REFUSAL-OPEN changelog draft has a Fixed
bullet saying that "current writable serving generation unavailable" now
carries a hint. That change (ee5415ca) was narrowed in this package and the
answer is unchanged, so drop that bullet.
