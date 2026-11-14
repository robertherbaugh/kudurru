# FLAKE-PRIMARY-UNAVAILABLE: bullets for `CHANGELOG.md` `[0.2.0]`

Only the first two bullets are user-facing. The last two are test fixes for
contributors; drop them if the changelog stays product-only.

### Fixed

- SQLite store: when write authority changes (a cluster activation, a fence by
  a higher term, a promotion conflict, an uncertain enrollment completion, or
  closing the store), the current leadership term now ends before the new
  authority becomes visible. Before, the primary driver or the serving writer
  could see its writer token retired a moment before its term ended, and the
  node logged a spurious `leader-only runnable failed; restarting` error and
  counted a restart in the runner status. No data was written in that window.
- Cluster runner: a leader-only task that stops with an error after its term
  has ended (for example `primaryruntime: required adapter/publication
  unavailable` from the primary driver) no longer leaves that error as the
  task's last error in the runner status.
- Tests: the dnsdist supervisor lifecycle test no longer restarts the fake
  dnsdist twice more than expected on a loaded machine. Its health window was
  shorter than one round trip of the fake console client.
- Tests: the TOTP confirm, factor rotation and local-user login tests no
  longer get 401 when the request crosses a 30-second step boundary. They took the "previous step" code from the wall
  clock instead of the server's clock.
