### Fixed

- A managed-anchor primary that was just restarted, promoted or chosen by
  `cluster resolve`, and whose root-anchor refresh is not due, no longer
  refuses writer commands with 503 "current writable serving generation
  unavailable" while its anchor updater makes its first step: it starts its
  writer services at once. A writer whose refresh is due (for example after a
  long outage) still finishes that refresh first, so a root key revocation it
  observes is committed instead of being lost to a startup write; such a node
  can refuse writer commands for up to 30 seconds after it reports ready
  (D369).
- A root anchor capture that raced an ordinary commit now logs its own
  consecutive count ("root anchor capture raced an ordinary commit; capturing
  again") instead of the update-race count, and only at the first, second,
  fourth, eighth and so on consecutive race, so a capture that keeps racing no
  longer logs a warning every second.

### Changed

- The enrolled offline recovery acceptance (`anchors recover --offline` on the
  stopped primary, the follower adopting the recovered image through scheduled
  pulls) now runs in the default end-to-end suite in minutes instead of only as
  an hour-long opt-in soak. Test binaries built with `-tags licensetest` accept
  `KUDURRU_TEST_ANCHOR_UPDATER_AHEAD`; release binaries do not.
