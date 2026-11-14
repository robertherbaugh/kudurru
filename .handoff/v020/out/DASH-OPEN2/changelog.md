### Added

- `kudurru records add --view public|<group>` stores a record as a publish-only or client-group view, and `kudurru records rm --view -|public|<group>` removes one view only; with `-n` the removal is limited to that namespace's record sets. `records list -o wide` shows each record's view.
- The dashboard lists a service account's API tokens (Tokens… on its row) and revokes one after a confirmation; listing needs `tokens.read` and revoking `tokens.delete`.
- The role binding form edits bindings with several subjects (up to 256) and adds or removes subjects; a subject listed twice is refused before saving.
- The Integrations list can delete several selected instances at once (`POST /integrations:batchDelete`), after a confirmation that names each instance's deletion policy.
- `GET /iam/users` and `GET /iam/serviceaccounts` name each account's manager (`managedBy`).
- `GET /zones/{zone}/records` says which list it returned (`source: desired` or `served`).

### Changed

- On the complete SQLite runtime a served record row names its record set (and its manager, set identifier, comment and labels) when exactly one stored record has its name, type and view.
- Local accounts and service accounts managed by the configuration file, the bootstrap or the system show their change actions disabled with the reason instead of being refused by the server.
- The RPZ feed state on the Lists page and on a list's page is reread every 3 s only at first while a feed stays expired, unavailable or pending, then less often up to every 30 s; a change of the feed starts the fast rereads again.

### Fixed

- Editing a served client-group view of a non-default namespace in the dashboard no longer saves it into the default namespace's group of the same name; a served row that names no record set can no longer be edited into a second, conflicting record.
- The role binding and token revocation dialogs no longer mention the revocation-record limit, which applies only to accounts and identity providers.
