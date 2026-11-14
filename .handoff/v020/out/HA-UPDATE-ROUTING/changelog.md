### Added

- An enrolled SQLite follower now forwards a TSIG-signed RFC 2136 UPDATE to
  its primary. The follower checks the zone and the signature, and the primary
  applies its own update policy and prerequisites and commits once. The
  follower returns the primary's answer (`NOERROR`, `NXRRSET`, `REFUSED` and
  so on), signed with the request's key. Update clients can use any member.
  The key must have the same name, algorithm and secret on every node.

### Changed

- When a follower cannot reach its primary, or loses the connection after
  sending an UPDATE, it answers `SERVFAIL` and never retries the update. RFC
  2136 clients then try their next server. Fenced, conflicted and static
  nodes still answer `SERVFAIL` without forwarding. A follower now answers
  `REFUSED` for an unsigned or unknown-key UPDATE and `NOTAUTH` for a zone it
  does not serve as a primary zone, as the primary does. Before this change
  it answered `SERVFAIL`. Member clocks must agree within a second: a
  forwarded update expires at most four seconds after the follower sends it.
  During a burst of updates, a few can be answered `SERVFAIL` while the
  primary republishes its previous commit. They are not applied and are safe
  to retry.
