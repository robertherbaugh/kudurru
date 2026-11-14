### Changed

- A signed RFC 2136 UPDATE sent through an enrolled SQLite follower gets a
  retry identity derived from the exact signed message. When a client sends
  the same signed message again after a `SERVFAIL` or a timeout, through the
  same follower or another one, the primary answers it with the original
  `NOERROR` if the first one was applied, instead of deciding it again (D381).
- A large `kudurru apply` (or `POST /api/v1/apply`, `:batchDelete`) through an
  enrolled SQLite follower is no longer cut by the follower's own next pull:
  the follower holds its scheduled pull, for at most 30 seconds, while a
  forwarded request runs. One file of 200 documents through a follower is
  applied completely. A pull after another sign-in, sign-out or settings
  change through the same follower can still cut it (D375).

### Fixed

- The primary no longer answers `SERVFAIL` for a forwarded UPDATE that it
  committed but could not publish at once: it waits for its next publication,
  within the four seconds of the forward, and answers the commit's own
  `NOERROR`. A forwarded UPDATE answered `SERVFAIL` is now logged at warning
  level with its stage and cause on the follower and the primary (D381).
- A forwarded UPDATE whose retry identity was already recorded but whose
  admission refused it is answered at once instead of waiting out the
  four-second bound (D381).
