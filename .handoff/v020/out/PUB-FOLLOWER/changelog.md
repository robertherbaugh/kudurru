### Fixed

- An apply, batch deletion or single write through an enrolled follower is no
  longer cut with "the enrolled primary is unavailable; the write was not
  applied" when the follower publishes the data it already has once more
  (after most pulls on a follower with managed root anchors). The follower now
  checks that it still serves the data the request began on, not the exact
  publication; a pull, a fence or a changed dependency still ends the request
  as before.
