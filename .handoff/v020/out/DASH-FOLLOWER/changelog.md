### Added

- Dashboard: on an enrolled SQLite follower, a banner above every page (for every signed-in role) names the primary and says which changes are sent there; the Nodes page shows the node's role, the primary and how many kinds of change the follower forwards or runs itself.
- Dashboard: controls whose change only the primary carries out (allow and deny entries, a block list's source entries, list refresh, Send NOTIFY, imports and decommissioning, authenticator setup, external account and authenticator recovery commands, unblock decisions on the Requests list and a request's page) are disabled on a follower, with a tooltip that names the primary. Everything the follower forwards stays available.
- Dashboard: the login page of a follower says that single sign-on runs on the primary.

### Changed

- Dashboard: `503 primary_required` is shown as "Needs the primary" with the primary's name, and `503 completion_unknown` as "Outcome unknown", asking to reload and check before trying again.
- Dashboard: after any successful change sent from a follower's dashboard (dialogs, wizards and row actions alike), the dashboard reads its pages once more about six seconds later, so a forwarded change shows once the follower has pulled it.
- Dashboard: the node's role is reread every 30 seconds and at once when a change answers `primary_required`, so an open tab follows a promotion or demotion.
- Dashboard: Settings save errors use the same "Needs the primary" and "Outcome unknown" wording.

### Fixed

- Dashboard: a sign-out that the server could not confirm (for example on a follower that cannot reach its primary) no longer fails silently; the login page says that the session may stay valid until it expires. A sign-out that cannot reach the server at all keeps you signed in and says that it failed, because the session cookie was not cleared.
