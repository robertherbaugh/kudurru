### Fixed

- Managed root anchors (RFC 5011, `anchorMode: managed`) on enrolled SQLite
  clusters: a member that returns after a promotion, or that lost an offline
  conflict resolution, now takes the current primary's trust-anchor state on
  its next scheduled pull even when its own state had been updated as often or
  more often. It previously refused the primary's data and stayed fenced until
  the primary's trust-anchor state had been updated more times than its own,
  which takes about a day at the real root and never happens if that primary's
  updater has stopped (D369).

### Changed

- Managed root anchors: a returning member never trusts a root key again that
  it saw revoked. If the current primary has not recorded that revocation yet,
  the member refuses the primary's data, stays fenced and keeps answering DNS
  from its own last-good data, which already treats the key as revoked; it
  follows the primary on the first scheduled pull after the primary's own
  root-anchor check records the same revocation. If the primary can never
  record it, replace the member with a newly joined receiver. A returning
  member whose `anchors recover` history the current primary does not share,
  or whose managed trust anchors the current primary never started, also
  refuses that primary's data and stays fenced on its own last-good DNS; see
  "A member returns with different trust-anchor state" in
  `docs/deploy/sqlite-join.md` (D369).
