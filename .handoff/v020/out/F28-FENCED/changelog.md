### Fixed

- Managed root anchors (RFC 5011, `anchorMode: managed`): a SQLite member that
  is fenced or holds an unresolved promotion conflict now keeps answering DNS
  from its last valid local data and its last accepted trust anchors, and
  restarts in that state, with `managedTopology` `direct` and `edges`. It
  previously answered `SERVFAIL` for every query, authoritative data included,
  and refused to start with "root anchor policy unavailable or changed". Every
  follower passes through the fenced state at each promotion, so this also
  removes that outage window (D369).

- Managed root anchors: a member whose clock is up to five minutes behind the
  primary no longer loses its root-anchor revocation checks, and a newly
  promoted primary no longer loses trust-anchor updates, for the rest of the
  term. The member waits for its own clock, keeps waiting while ordinary
  replicated changes arrive, and logs a `check time synchronization` warning.
  A larger gap is still refused with `invalid clock continuity` (D369).

### Changed

- Managed root anchors: fenced and conflicted members keep their own
  deny-only check of the root DNSKEY set (about hourly, outbound TCP 53 to the
  configured root servers), like followers. A signed revocation retires their
  DNS output until they install newer trust-anchor state from the primary. They
  never update or bootstrap trust anchors, and their HTTP API stays closed
  (D369).
