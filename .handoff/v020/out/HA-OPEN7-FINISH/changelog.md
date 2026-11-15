<!-- HA-OPEN7-FINISH: replaces out/HA-OPEN7/changelog.md as a whole. -->

### Changed

- A signed RFC 2136 UPDATE sent through an enrolled SQLite follower gets a
  retry identity derived from the exact signed message. When a client sends
  the same signed message again over the same transport after a `SERVFAIL`
  or a timeout, through the same follower or another one, the primary
  answers it with the original `NOERROR` if the first one was applied,
  instead of deciding it again (D381).
- A large `kudurru apply` (or `POST /api/v1/apply`, `:batchDelete`) through
  an enrolled SQLite follower is no longer cut by the follower's own next
  pull: the follower holds its scheduled pull, for at most 30 seconds from
  when it was due, while a forwarded request runs. The request is still
  bounded by its 30-second lifetime, and each document is one exchange in
  which the primary commits it, so how many documents fit depends on the
  primary (200 DNS record sets took 12 to 30 seconds in the release's
  process tests). A pull after another sign-in, sign-out or settings change
  through the same follower can still cut it. Send a large file, and every
  restore, to the primary (D375).

### Fixed

- The primary no longer answers `SERVFAIL` for a forwarded UPDATE that it
  committed but could not publish at once: it waits for its next
  publication, within the four seconds of the forward, and answers the
  commit's own `NOERROR`. A forwarded UPDATE answered `SERVFAIL` is now
  logged at warning level with its stage and cause on the follower and the
  primary (D381).
- A forwarded UPDATE whose retry identity was already recorded but whose
  admission refused it (for example, the zone stopped allowing updates) is
  answered `SERVFAIL` at once instead of going unanswered after the
  four-second bound (D381).
