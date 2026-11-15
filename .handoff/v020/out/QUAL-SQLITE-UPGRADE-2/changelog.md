# CHANGELOG.md `[0.2.0]` bullets from QUAL-SQLITE-UPGRADE and QUAL-SQLITE-UPGRADE-2 (D373)

These replace the bullets of `out/QUAL-SQLITE-UPGRADE/changelog.md`.

### Changed

- **A zone that cannot be served is refused when it is written (SQLite:
  Docker, systemd, the chart's `standalone` mode).** A DNSZone of a type no
  release serves yet (`external`, `catalog`, `secondaryCatalog`), with an
  entry in `spec.policy.queryACL` or `transferACL` that is not an address or
  a network, with a notify target that is not an address, a primary zone
  whose `spec.policy.update` is not `deny` or `allow`, a primary zone with a
  `spec.policy.updatePolicies` entry that cannot be compiled (a `name`
  outside the zone, a type that does not exist or that an update cannot
  carry, an empty `tsigKeyRef`; also when updates are denied), a zone with
  the `spec.zoneName` of another DNSZone, or a zone created under a new
  object name for the zone name of a zone deleted on this node (re-create it
  under its earlier name) is no longer stored. The API answers
  `400 invalid` and names the field; the file loader logs the object as
  invalid and loads the rest. 0.1 stored such a zone and reported it on the
  zone, or (for update grants) stored it without using it. Kubernetes is
  unchanged: the apiserver admits the object and the controller reports it.
- **An upgrade checks what 0.1 stored before it changes anything (SQLite).**
  If `state.db` holds an object that 0.2 cannot serve, kudurru does not
  start: it names every such object, leaves the data directory exactly as
  0.1 wrote it, and says what to do. 0.1 still runs on the directory, and the
  objects are corrected there. See "Upgrade refused: stored objects are not
  valid in this release" in `docs/deploy/runbooks.md` for the list: secondary
  and stub zones without their configuration, zones of the types above,
  several DNSZones for one zone name, zone ACLs that do not parse, update
  modes other than `deny` and `allow`, update grants that cannot be
  compiled, and policy rewrites that are not a valid rewrite.
- A `querylog.db` written by a newer release is refused with the same message
  as `state.db` and `local.db`: `querylog.db was written by a newer kudurru`.
- The server log says why a serving generation is not published, with the
  object and the reason, once per failure (`complete serving generation not
  published`). Readiness and the API still report only that it is
  unavailable.

### Fixed

- **One object could stop a whole SQLite node.** A stored DNSZone that the
  serving image could not build (see above), a zone re-created under
  another object name, any record set whose `zoneRef` named a forward zone,
  or a block list whose URL had never downloaded was accepted and then
  stopped every DNS answer, readiness and the whole API, across restarts,
  with no API left to remove the object. The zones are now refused; a
  record set that names a forward zone is stored and reported on the record
  set (`forward zones hold no records`), as in 0.1, and the zone is served;
  a block list source that has never downloaded is served empty and the
  list reports the failed fetch, as in 0.1.
- **A record set the zone cannot hold no longer freezes its zone (SQLite).**
  A record set with an endpoint that does not parse (for example an `A`
  target that is not an address, an unknown record type or an `MX` without
  a preference), with two endpoints for one name and type, or with an
  endpoint the zone refuses (a CNAME next to other data, a CNAME or NS
  endpoint at the apex, data below a DNAME, two DNAME targets for one name
  with different set identifiers) kept every later change of its
  zone from being served, and after an upgrade from 0.1 kept the zone, and
  readiness, from coming up at all. What the zone cannot hold is now left
  out and reported on its record set, as in 0.1, and the rest of the zone
  is served. Of two DNAME endpoints for one name with different set
  identifiers, which 0.1 served together, the first is kept and the
  second is reported. A record set that was valid and is changed into one that is
  invalid as a whole is now left out of the zone, as in 0.1 and on
  Kubernetes, instead of keeping its earlier records.
- **A block list whose URL does not answer no longer takes the node down
  (SQLite).** While the first download of a list source had not finished,
  for example because its URL accepted the connection and never answered,
  no serving generation was published: no DNS answers, readiness 503 and
  every API route 503 until the download attempts gave up (more than five
  minutes in the run that found it), so the list could not even be
  removed. The source is now served empty until its first download
  publishes a body. A failed download is reported on the list
  (`ListFetchFailed`); while the first download is still running nothing is
  reported yet, for up to three attempts of at most five minutes each.
- **A zone serial no longer changes on the first start after an upgrade
  (SQLite).** The status of a zone that this release had not committed yet
  could be rewritten with serial 0 before the zone's first commit, which
  then gave the zone a new serial although its content was the same as
  under 0.1 (secondaries would transfer it again). The serial 0.1 served
  is now kept.
- A start that refuses a `querylog.db` written by a newer release no longer
  rewrites the file's header: the file is left byte for byte as the newer
  release wrote it.

### For the upgrade notes (verified with the released 0.1.0 binary)

- Back up first. An upgrade cannot be undone: `state.db` goes from schema 1
  to 9, `querylog.db` from 2 to 3 and the `local.db` marker from 1 to 9, and
  the 0.1.0 binary then refuses the directory (`state.db was written by a
  newer kudurru`) without changing it.
- What carries over unchanged, in the directories the upgrade tests use:
  every answer (authoritative, blocked, allowed, denied and forwarded names
  over UDP and TCP, including record sets that are invalid as a whole,
  record sets with endpoints the zone cannot hold, and a block list whose
  URL never downloaded), zone serials, object UIDs and generations, API
  tokens (break-glass, personal and the bootstrap token), the admin
  password, the query log with its row ids, and the audit trail, which
  verifies across the upgrade.
- A setting saved from the Settings page of 0.1 (in the `settingsFile`) is
  the effective setting after the upgrade. From then on cluster settings are
  kept in `state.db`: a later change is not written to the settings file, and
  the file no longer overrides it at a restart.
- The response cache starts empty: the cache dump of 0.1 is not read
  (`cache dump not restored`, a warning at the first start).
- A secondary or stub zone that 0.1 stored with its configuration was not
  served by 0.1 and is served by 0.2.
- For a moment after readiness turns 200 (about half a second, longer
  under load) the node still commits its own zone status, and an API write
  that arrives during such a commit, or while the generation of an earlier
  write is being published, is answered `503 unavailable` (or
  `generation_pending`) without effect. Automation should repeat such a
  request.
- A primary zone with a child zone can answer `SERVFAIL`, and readiness can
  stay 503, for up to about a minute on the first start after the upgrade,
  until parent and child have committed their first generation.
