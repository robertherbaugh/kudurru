### Changed

- On SQLite, publishing a new configuration repeats less work: the parts of
  the node that read the same stored generation now reuse its digest, its
  canonical form and each object's identity instead of computing them again,
  which saves CPU during a long apply or restore. The cache is bounded by
  entry count and by bytes. A heavily loaded node can still refuse other
  callers' reads with `503 generation_pending` while one long request runs.

(No separate bullet for the zone `SERVFAIL` window during a long apply: its
cause was fixed by the status-write change, which has its own bullet in
`out/CORE-STATUS-WRITE-SERVFAIL/changelog.md`. The test and comment changes
of this package are not user-facing.)
