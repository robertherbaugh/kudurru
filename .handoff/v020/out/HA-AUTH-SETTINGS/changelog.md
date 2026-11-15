### Added

- An enrolled SQLite follower forwards sign-out (`POST /auth/logout`, `POST /auth/logout/browser`) and the revocation of one of your own sessions (`DELETE /auth/sessions/{id}`) to its primary. The primary checks the credential again and ends exactly the session that the cookie belongs to; the follower clears the browser's cookies and refuses the session as soon as it installs the primary's state. If the primary cannot be reached, sign-out still clears the cookies and answers `503 unavailable` or `503 completion_unknown`, saying that the session may stay valid until it expires (D368).
- An enrolled SQLite follower forwards cluster settings changes (`PATCH /settings`, the dashboard's Settings page) to its primary, which repeats every check of its own route. A change to settings that changed on the primary since the follower installed them answers `409 conflict` and is not applied. The follower answers a committed change once it serves the new values, so the next change through it, such as the next key on the Settings page, is not refused. Node settings (`PATCH /node/config`) stay primary-only (D368).

### Changed

- `GET /cluster` on a follower now lists `logout`, `browserLogout`, `revokeSession` and `patchSettings` among the forwarded operations, the OpenAPI document marks them `x-kudurru-follower: forwarded`, and a follower's settings schema no longer marks cluster keys read-only.

### Fixed

- Signing out on an enrolled follower answered `503` and left both the cookie and the durable session valid, although the session had been created on that follower.
