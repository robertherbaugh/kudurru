### Added

- An enrolled SQLite follower now forwards your own password change
  (`POST /auth/password`) and the creation and revocation of personal API
  tokens (`POST /auth/tokens`, `DELETE /auth/tokens/{id}`) to its primary. An
  account that must change its password can do so on the follower it signed in
  to, and the new cookie works on every member. The new password and a new
  token's secret never travel to the primary: the follower sends only their
  hashes, and the primary applies the shared lockout counters and every check
  of its own route. The follower pulls the primary's state once and answers
  when it serves the result itself, so a new cookie or token works there at
  once. A password change that committed is never reported as `unavailable`:
  if the follower cannot give a browser its new cookie in time it answers
  `503 committed_output_unavailable`, which means that the password changed
  and you sign in with the new one (D368).

### Changed

- `kudurru login --username` works against an enrolled follower and no longer
  reads `GET /auth/status` first to refuse there.
- The allow and deny list commands and unblock request decisions stay
  primary-only on a follower in this release (`503 primary_required`); change
  such a list through a follower by writing its BlockList object, which is
  forwarded.
