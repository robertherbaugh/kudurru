<!-- INT-OPEN (D367 follow-up). Fold into `CHANGELOG.md` `[0.2.0]` next to the
     INT-DEL-C bullets. It refines the INT-DEL-C Fixed bullet "A `Delete`
     cleanup run that a slow store keeps from finishing in its 30-second bound
     is reported as `CleanupFailed` ..."; keep that bullet and add this one. -->

### Fixed

- A `Delete` cleanup run whose provider request does not answer within the
  run's 30-second bound is reported as `ProviderFailed` again, not as
  `CleanupFailed`, even when the provider returned a partial answer that
  could not be recorded in time. The retry backoff is unchanged.
