### Added

- Metric `kudurru_root_anchor_updater_acquisitions_total{outcome}` counts the
  root-anchor updater's queries to the root servers as `applied`, `raced` (an
  ordinary write landed while the reply was in flight, so the node asked
  again), `failed` or `refused`.

### Changed

- A managed primary whose root anchors are not due for a refresh starts its
  writer services (and accepts writer commands) without waiting for its first
  root-anchor step; a due refresh still runs first, so a revocation it observes
  is committed before ordinary writes (carried from F28-OPEN6).

### Fixed

- Cluster members with managed root anchors no longer answer `SERVFAIL` to
  validated recursion and zone queries while they persist a newly observed
  fence, promotion conflict or divergence. DNS keeps answering from the served
  publication and accepted trust until the DNS-only publication takes over;
  writers and the HTTP API still close at once, and a fence that cannot be
  written to disk still stops DNS as before.
