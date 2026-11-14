### Fixed

- A sign-in, sign-out, cluster activation, invitation, object, zone or
  settings write that a ready node has already read is no longer refused
  with `503 unavailable` ("current writable serving generation
  unavailable") only because one of the node's own background commits
  (zone status and lifecycle writes, signed UPDATEs) published while the
  command ran. The command waits up to five seconds for the newer
  publication, checks the caller's credential, session, roles and the
  cluster settings again on it, and is applied once. If the node keeps
  publishing for five seconds, or the caller's access changed meanwhile,
  it still answers `503 unavailable`, now with `Retry-After: 1`, which
  means that no part of the command ran. This does not change the other
  refusal: a request that arrives before the node has published its newest
  commit is still refused before it is read with `503 generation_pending`
  (`Retry-After: 1`), also on a ready node that commits steadily, and
  clients should send it again.
