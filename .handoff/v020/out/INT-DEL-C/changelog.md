<!-- INT-DEL-C (D367, part 3 of 3). Fold these into `CHANGELOG.md` `[0.2.0]`
     together with the INT-DEL-A and INT-DEL-B bullets. They supersede this
     sentence of the INT-DEL-B "Added" bullet: "Everything it needs (the
     finalizer, the ledger and the recorded operations) is stored, so it is
     designed to continue from there after a restart or failover; this release
     tests that at the level of the integration host, not yet with a whole
     server restarting mid-cleanup." Replace it with the first bullet's
     restart/failover/license sentence below. They also supersede, in the
     INT-DEL-B bullets, "An instance's operation history of any length is
     forgotten in bounded steps." (replace it with the first Fixed bullet
     below) and the whole "Known limit of the `Delete` cleanup ... 200,000
     rows of one kind ..." bullet (replace it with the last Changed bullet
     below): the 200,000-row scan bound cannot be reached, because the store
     limit of 100,000 objects stops every Integration job first. -->

### Added

- `kudurru integrations release NAME --uid UID --revision REV` (and
  `POST /api/v1/integrations/{namespace}/{name}/release`) releases a
  `Terminating` Integration without finishing its cleanup: the exact reviewed
  object is removed, its provider records and ledger rows stay as under
  `Retain`, and no provider is contacted. It needs neither a license nor the
  credential, refuses a live object or a changed UID or revision with
  `409 Conflict`, needs the new `integrations.release` permission (admin
  only), and is recorded in the audit log as `integration.release` with what
  was retained (D367).
- Integration summaries (`GET /api/v1/integrations`, `kudurru integrations
  get` and `list`) show `deletionPolicy`, `terminating`,
  `deletionRequestedAt` and, while the cleanup reports, its `remaining`,
  `blocked` and `uncertain` counts. `kudurru integrations list` has the new
  columns POLICY, REASON and DELETION REQUESTED.
- A `Delete` cleanup now continues after a server restart and after a
  failover to another elected writer, and pauses without any provider
  request while the license is expired, resuming when a valid license is
  installed. A delete whose answer was lost is never sent again; the next
  writer confirms it by rereading the zone.

### Changed

- The `spec.deletionPolicy` field description in the Integration CRD now
  states what `Retain` and `Delete` do, and that the policy has no effect on
  Kubernetes in this release.

- The Integrations guide now names what bounds an Integration's stored
  history: the SQLite store as a whole. Above 100,000 stored objects or
  256 MiB of stored documents, all kinds together, the store's
  whole-generation read refuses and no Integration job runs, a `Delete`
  cleanup included. Every record change of every Integration, including
  removed `Retain` ones, leaves one stored operation that counts toward it.

### Fixed

- The `Delete` cleanup of an Integration with a long record-change history
  no longer stalls when the store is slow: it forgets the history in commits
  sized to fit its 30-second run and makes them smaller when one does not
  fit, and is tested on SQLite with a history of several stored pages
  (D367).
- A `Delete` cleanup run that a slow store keeps from finishing in its
  30-second bound is reported as `CleanupFailed` and retried with backoff,
  instead of `ProviderFailed`.
- A cleanup whose first count of the ledger fails keeps the counts it last
  reported instead of showing nothing remaining.
- A `Delete` cleanup whose provider delete was cut before its receipt no
  longer stays `CleanupFailed` until the next resync: the receipt confirmed by
  a reread is recorded in UTC and accepted at once.
