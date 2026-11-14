# CHANGELOG.md `[0.2.0]` bullets from QUAL-SQLITE-UPGRADE (D373)

### Changed

- **A zone that cannot be served is refused when it is written (SQLite:
  Docker, systemd, the chart's `standalone` mode).** A DNSZone of a type no
  release serves yet (`external`, `catalog`, `secondaryCatalog`), with an
  entry in `spec.policy.queryACL` or `transferACL` that is not an address or
  a network, with a notify target that is not an address, a primary zone
  whose `spec.policy.update` is not `deny` or `allow`, a zone with the
  `spec.zoneName` of another DNSZone, or a zone created under a new object
  name for the zone name of a zone deleted on this node (re-create it under
  its earlier name) is no longer stored. The API answers
  `400 invalid` and names the field; the file loader logs the object as
  invalid and loads the rest. 0.1 stored such a zone and reported it on the
  zone. Kubernetes is unchanged: the apiserver admits the object and the
  controller reports it.
- **An upgrade checks what 0.1 stored before it changes anything (SQLite).**
  If `state.db` holds an object that 0.2 cannot serve, kudurru does not
  start: it names every such object, leaves the data directory exactly as
  0.1 wrote it, and says what to do. 0.1 still runs on the directory, and the
  objects are corrected there. See "Upgrade refused: stored objects are not
  valid in this release" in `docs/deploy/runbooks.md` for the list: secondary
  and stub zones without their configuration, zones of the types above,
  several DNSZones for one zone name, zone ACLs that do not parse, update
  modes other than `deny` and `allow`, and policy rewrites that are not a
  valid rewrite.
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
- **A record set endpoint the zone cannot hold no longer freezes the zone
  (SQLite).** A CNAME next to other data, or a CNAME or NS endpoint at the
  zone apex, kept every later change of its zone from being served (and,
  after an upgrade from 0.1, kept the zone from being served at all). The
  endpoint is now left out and its record set reports it, as in 0.1.

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
  over UDP and TCP, including a record set with endpoints the zone cannot
  hold and a block list whose URL never downloaded), zone serials, object UIDs and
  generations, API tokens (break-glass, personal and the bootstrap token),
  the admin password, the query log with its row ids, and the audit trail,
  which verifies across the upgrade.
- A setting saved from the Settings page of 0.1 (in the `settingsFile`) is
  the effective setting after the upgrade. From then on cluster settings are
  kept in `state.db`: a later change is not written to the settings file, and
  the file no longer overrides it at a restart.
- The response cache starts empty: the cache dump of 0.1 is not read
  (`cache dump not restored`, a warning at the first start).
- A secondary or stub zone that 0.1 stored with its configuration was not
  served by 0.1 and is served by 0.2.
- A primary zone with a child zone can answer `SERVFAIL`, and readiness can
  stay 503, for up to about a minute on the first start after the upgrade,
  until parent and child have committed their first generation.
