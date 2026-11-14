### Fixed

- A primary zone and a primary child zone under it on one node (for example
  `example.` and `child.example.`) no longer leave one or both answering
  SERVFAIL until the process restarts. They converge in either order of
  creation and across deletion and re-creation of the child, and the parent
  keeps answering its own names while its child is being added.
- A new zone, and in particular a new child zone, starts answering sooner on a
  node with many zones: the zone driver now takes a zone that is being created
  through its steps first instead of queuing each step behind every other
  zone. Until its first publication, names under a new child zone answer
  SERVFAIL (which resolvers retry within seconds), not the parent's negative
  answer (which resolvers would cache for the parent's negative TTL).
