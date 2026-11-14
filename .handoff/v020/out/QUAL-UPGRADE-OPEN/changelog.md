# CHANGELOG.md `[0.2.0]` bullets from QUAL-UPGRADE-OPEN (D373)

These add to the bullets of `out/QUAL-SQLITE-UPGRADE-2/changelog.md` and
`out/QUAL-KUBE-UPGRADE/changelog.md`; nothing there needs to be removed.

### Changed

- Of two CNAME targets for one name that differ only in their set
  identifier, the first is kept and is now served with its own TTL. 0.1
  served it with the lower TTL of both targets, so a zone with that shape
  gets a new serial on its first commit after the upgrade.

### Fixed

- **A record set at a name that a dynamic update owns no longer freezes its
  zone (SQLite).** A DNSRecordSet with an endpoint for a name that already
  held a record added by a dynamic update (RFC 2136) kept every later change
  of the zone from being served while the dynamic record existed. Only that
  endpoint is now left out; the record set's other endpoints are served,
  and a record set with nothing served is reported `Duplicated` in the
  zone's source diagnostics. The dynamic record keeps answering and the rest
  of the zone takes changes as before. Delete the dynamic record or the
  endpoint to resolve it.
- **A block list whose published body a node lacks no longer stops serving
  (SQLite).** When a list's status named a published digest but the node's
  `lists` directory held no body for it, and the list URL did not answer,
  the node published no serving generation (no DNS answers, readiness 503)
  until the body arrived. The list is now served empty until its body is
  stored, and then blocks again. While it waits the list does not block on
  that node, and the node logs this once for the source.
- A list URL that accepts the connection and never answers no longer
  delays the status of other block lists, or the first download of a list
  added while it hangs; each list's status is written as soon as its own
  downloads end.
