### Fixed

- A burst of signed RFC 2136 updates sent through an enrolled SQLite follower,
  as a DHCP server sends them, is now answered `NOERROR` for every update the
  primary accepts. Before, once a commit was being republished, most of the
  remaining updates in the burst were answered `SERVFAIL` and not applied. An
  update that reaches the follower or the primary while its publication is
  being replaced now waits for the next one, within the forward's four
  seconds, and is checked again there; nothing had been sent or applied, so
  this is not a retry. An update sent to the primary directly still can be
  answered `SERVFAIL`, without being applied, right after a commit (D381).
- Through a follower, an item of `kudurru apply`, `kudurru restore` or a
  dashboard multi-delete that the follower cut because its data changed now
  says what to do: it names the enrolled primary and asks to send the items
  that were not applied there, because running the file again through the
  follower stops the same way (D375).
- A forwarded write whose answer the primary sent, but which the follower
  could not relay because its own data or membership changed during the
  exchange, is now answered `503 completion_unknown` ("the write may have been
  applied") instead of `503 unavailable` "cluster authority changed", which
  claimed that nothing was made (D375).
- A token caller's password change through a follower is always answered for
  the caller's account, as on the primary, whether or not the calling token
  still works afterwards, and it waits for the follower's next publication
  instead of answering `503 committed_output_unavailable` while one is being
  prepared; a browser session the follower could not issue after a committed
  change now names the cause in its `committed_output_unavailable` answer
  (D368).
