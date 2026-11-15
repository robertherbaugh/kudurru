### Fixed

- A primary zone and a primary child zone under it on the same node (for
  example `example.` and `child.example.`) no longer leave the parent or the
  child answering SERVFAIL until the process restarts (readiness reported
  `zone awaiting committed publication`). This could happen whichever zone
  was created first, and when the child was deleted and created again.
- Creating a primary child zone no longer makes its primary parent answer
  SERVFAIL until the child is set up: the parent keeps answering its
  committed data for the names outside the child. Creating a primary parent
  over an existing primary child no longer makes the child answer SERVFAIL
  until the parent is active.
