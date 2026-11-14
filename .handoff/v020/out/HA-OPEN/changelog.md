### Changed

- A blocking switch (`POST /dns/blocking`) sent through an enrolled SQLite follower whose view of the cluster settings is older than the primary's now answers the primary's `409 conflict` and changes nothing, instead of `503 unavailable` with the text "authentication operation unavailable". A switch the follower could not send answers `503 unavailable` and says that nothing was applied; one whose exchange with the primary failed answers `503 completion_unknown` (D368).
- A follower whose connection to its primary is refused, or whose primary's host is reported unreachable, now answers a forwarded change (object writes, zone commands, settings, sign-out, the blocking switch) with `503 unavailable` and "was not applied", because nothing was sent. `503 completion_unknown` remains the answer when the connection was made and then failed, or when the connection attempt was still waiting at the request's deadline (a host that silently drops packets), because the follower cannot rule out that the primary committed the change (D368).
- A member fenced by data that diverges from its primary's at the same revision still answers `503 cluster_conflict` with the same offline hint, but its message and the CLI hint now name the divergence instead of "conflicting promotions" (D368).

### Fixed

- On a follower, fenced or conflicted node, the block page again shows a client's pending or declined unblock request with its status. The request form stays hidden there, and the page no longer offers to add details or ask again (D368).
