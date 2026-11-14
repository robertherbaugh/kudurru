<!-- INT-DEL-A (D367, part 1 of 3). Fold these into `CHANGELOG.md` `[0.2.0]`.
     If parts 2 and 3 of the Delete lifecycle land, their entry ("Integrations with
     `deletionPolicy: Delete` remove their published records ...") supersedes the
     last sentence of the first Changed bullet below. -->

### Added

- Integrations declared with `deletionPolicy: Delete` are stored with the
  reserved finalizer `kudurru.io/integration-cleanup`. On SQLite it is set in
  the same write as the object; no edited, re-declared or force-applied
  document can add or remove it, and an object that was stored without it is
  armed by the next load of its unchanged document. On Kubernetes nothing sets
  the finalizer yet, because the integration host does not run there; the
  store driver already preserves one through server-side apply and forced
  takeovers. This is the first part of the `Delete` cleanup lifecycle (D367).

### Changed

- Removing the document of a SQLite Integration that declares
  `deletionPolicy: Delete` no longer deletes the object at once: it stays
  visible with a deletion timestamp, and declaring it again with
  `deletionPolicy: Delete` does not cancel the deletion. Declaring it with
  `deletionPolicy: Retain`, or without a `deletionPolicy` line (`Retain` is
  the default), releases it without contacting the provider. The release is
  logged as `objects: finalized` and recorded in the audit log as
  `integration.finalize` with the reason `released without cleanup:
  deletionPolicy is Retain; dropped kudurru.io/integration-cleanup`; changing
  a live Integration from `Delete` to `Retain` is recorded with the reason
  `disarmed: ...`. Integrations with the default `Retain` policy are
  unaffected. Running a `Delete` instance is still refused in this step.
- The objects loader requests the deletion of a file-managed object once.
  While the object waits for its finalizers, later loads no longer count it
  in `kudurru_objects_applied_total{result="deleted"}` or log `objects:
  deleted` for it again.
