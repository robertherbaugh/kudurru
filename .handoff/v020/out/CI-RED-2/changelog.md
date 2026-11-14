### Fixed

- An enrolled SQLite follower no longer answers a signed dynamic update
  `REFUSED` when it installs the primary's previous commit while the update
  is on its way (for example during a burst from a DHCP server). It answers
  `SERVFAIL` without passing the update on, so the client sends it again; the
  primary answers the same way when its keyring changes under an update.
- With managed root trust anchors, a freshly installed node no longer stops
  its root anchor updater ("root anchor updater stopped for this writer
  term: replication: authority changed") when its first-boot setup commits
  while the updater starts. The updater now tries again a second later, so
  the node keeps refreshing its anchors without a restart.
