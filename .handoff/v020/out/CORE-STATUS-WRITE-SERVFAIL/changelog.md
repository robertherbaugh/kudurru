### Fixed

- A primary zone on the SQLite runtime keeps answering its committed records
  after every change to its data. In the half second or so after a record
  change or a dynamic UPDATE (while the zone's own status was being written),
  the zone, and a parent zone that delegates to it, used to answer SERVFAIL
  (`servfail_unsynced`).
