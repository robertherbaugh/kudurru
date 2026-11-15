### Fixed

- Managed (RFC 5011) root trust anchors: the elected updater no longer stops
  for the rest of the writer term when an ordinary change, such as a zone
  status update at startup, is committed while it reads or acquires the root
  key set. It discards that reply and makes a new acquisition, waiting 1, 2,
  4 and so on up to 64 seconds while the race repeats. Before this fix, a
  newly started managed node could be left without managed trust until it
  was restarted. Each such retry logs a warning ("root anchor update raced
  an ordinary commit") with the next attempt time and the consecutive count.

### Changed

- The opt-in live edge acceptance (`KUDURRU_ANCHOR_EDGE_LIVE=1`) now covers a
  primary and a newly joined follower as two backends of one pinned dnsdist
  edge. This qualifies the "Add a follower behind managed edges" runbook
  order.
