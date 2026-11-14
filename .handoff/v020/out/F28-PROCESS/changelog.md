### Added

- Managed root anchors (RFC 5011, `anchorMode: managed`): `GET /cluster/anchors`
  (`kudurru cluster anchors`, permission `cluster.read`) shows a node's root
  trust-anchor state: every root key with its DS key tag, RFC 5011 status,
  remaining hold-down and revocation time, quarantine, `anchors recover`
  records and the next scheduled refresh; the keys this member denied after its
  own root check and whether its output is retired; whether the elected updater
  or the deny-only root check is running, waiting for the clock, retrying or
  stopped (with the reason); and, on a member that pulls from the primary,
  whether it refuses the primary's trust-anchor state and why (a revocation it
  holds that the primary has not recorded, different recovery history, a
  primary without managed anchors, other seeds), and since when. Fixed warnings
  name each state that holds a member back. The route changes nothing (D369).
- Metrics `kudurru_root_anchor_worker_state{worker,state}`,
  `kudurru_root_anchor_adoption_refused{reason}` and
  `kudurru_root_anchor_admission_current`, so the same state is visible on a
  fenced or conflicted member, whose API is closed. A member that refuses the
  primary's trust-anchor state logs one warning when the refusal begins or
  changes and one line when it ends.
- `kudurru doctor` checks managed root anchors on a node that runs them,
  including a joined member that takes `anchorMode: managed` from the primary
  without setting it in its own file: it fails when every trusted root key is
  lost or the member's output is terminal, and warns on each status warning.

### Changed

- `kudurru anchors status --offline` also prints the owner's keys (DS key tag,
  status, first seen, revocation time), the recovery record generations, the
  member's local denials and its authority mode.
- The recovery runbook says how recovery works in an enrolled cluster: recover
  on the stopped primary only (a follower's database is refused before any root
  query), restart the primary, and followers take the recovered state on their
  next scheduled pull without a restart; restart a follower whose own root check
  had denied the lost key once it serves again, to resume its root check.

### Fixed

- Managed root anchors on enrolled SQLite clusters: a follower (or a fenced
  or conflicted member) no longer stops checking the root for revocations
  for the rest of its process when an ordinary change from the primary, such
  as the next member's join, is installed while its root check starts. It
  previously logged "root anchor follower stopped for this authority epoch"
  with "authority changed" and stopped revocation checks until a restart.
  Nothing had been sent to the root; the check now waits a second and starts
  again (D369).
