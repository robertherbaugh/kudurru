<!-- INT-DEL-B (D367, part 2 of 3). Fold these into `CHANGELOG.md` `[0.2.0]`.
     They supersede two sentences of the INT-DEL-A bullets: "Running a `Delete`
     instance is still refused in this step." (drop it) and "This is the first
     part of the `Delete` cleanup lifecycle (D367)." (the lifecycle now runs on
     SQLite). The API fields, the `release` command and the CLI columns are
     part 3 and are not claimed here. -->

### Added

- Integrations with `deletionPolicy: Delete` now run on SQLite, and deleting
  one cleans up before the object goes. Removing its document leaves the
  Integration visible with phase `Terminating`; the elected writer then
  deletes the records the instance provably published (every row carries its
  marker and the ledger lists every provider ID), forgets their ledger rows,
  removes the local state of its source in one commit (generated record sets
  and zones, inventory clients, name reservations, stored observations and
  deliveries), and removes the object. `Delete` overrides a zone's
  `upsert-only` or `create-only` policy for those records. Records of other
  owners, hand-made records and anything that only carries a copied marker are
  never touched. The cleanup works in bounded runs and shares the Cloudflare
  request budget. Everything it needs (the finalizer, the ledger and the
  recorded operations) is stored, so it is designed to continue from there
  after a restart or failover; this release tests that at the level of the
  integration host, not yet with a whole server restarting mid-cleanup. An
  uncertain provider answer is resolved from a complete reread, never by
  blindly repeating the write. An instance's operation history of any length
  is forgotten in bounded steps. The
  audit log records the purge (`integration.purge`) and the finalization
  (`integration.cleanup`) with what was deleted, forgotten and purged (D367).
- A cleanup that cannot finish keeps the Integration `Terminating` with a
  reason and counts in the integration status; the elected cleanup also
  writes them to the object's `Ready` condition and `status.records`. The
  number of records not yet removed is counted when each run starts, so a run
  that stops early still reports it. Blocking reasons: `RecordsConflict`,
  `OwnershipUnproven`, `PendingRecovery`, `DestinationRemoved`,
  `SnapshotIncomplete`, `LocalStateRemains` and `CapacityExceeded`. Paused or
  retrying: `CredentialsUnavailable`, `ProvisionFailed`, `ProviderFailed`,
  `ProviderUnavailable`, `RateLimited`, `BudgetExhausted`, `CleanupFailed`
  and `ReleaseFailed`. `LicenseUnavailable` and `Suspended` start no cleanup
  job, so they appear only in the integration status, and the object keeps
  the condition last written to it. Records that can be proven are still
  deleted; nothing is finalized. A blocked cleanup is checked again at the
  resync interval, at most every ten minutes. The cleanup of a UniFi or
  webhook instance makes no provider call and needs no credential.
- Declaring a `Terminating` Integration with `deletionPolicy: Retain` (or
  without the line) releases it without contacting the provider and without a
  license or credential, as before; its remote records and ledger rows stay.

### Changed

- A live Integration with `deletionPolicy: Delete` that is stored without the
  reserved finalizer `kudurru.io/integration-cleanup` is shown as `Paused`
  with reason `CleanupFinalizerMissing` and makes no provider call. The next
  load of its unchanged document adds the finalizer.
- Check, preview, sync and actions are refused for an Integration that is
  `Terminating`.
- On Kubernetes no Integration executes yet, so `deletionPolicy` still has no
  effect there.
- Known limit of the `Delete` cleanup: the local purge and the finalization
  each scan every row of the kinds they check in one transaction and stop at
  200,000 rows of one kind. A store whose Integrations together hold more stored record
  changes than that cannot finalize a `Delete` cleanup: the Integration stays
  `Terminating` with `CapacityExceeded` after its provider records are
  deleted, and must be released with `deletionPolicy: Retain`.
