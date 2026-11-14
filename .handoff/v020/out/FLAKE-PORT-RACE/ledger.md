### FLAKE-PORT-RACE test listener ports without the free-port race — November 12, 2026 (no decision number)

What now happens: tests that must write a listener address into a configuration
or onto a child's command line before the server binds it take the port from the
new test-only package `internal/testutil/testport` (`Reserve(t) int`,
`Address(t) string`). It allocates only from a window outside the kernel's
ephemeral port range (`net.ipv4.ip_local_port_range`, 32768-60999 here, so the
window is 20000-32767; on other systems the IANA range 49152-65535 is assumed).
The kernel therefore never gives a reserved number to a port-0 listener or to the
local end of an outgoing connection, which was the main way the old helper lost
its port. Each reservation also holds an exclusive non-blocking `flock` on
`<dir>/<port>.lock` plus an in-process table until the test's cleanup runs, so
no other test or process can be handed the same port while the server may still
bind it. `<dir>` is `KUDURRU_TESTPORT_DIR` or, when that is unset,
`$TMPDIR/kudurru-testport` as seen when the package initializes (before any
test runs). The package then exports the result as `KUDURRU_TESTPORT_DIR`, so a
test that later calls `t.Setenv("TMPDIR", ...)` before reserving (the remote
cache mount tests in `serve_remote_cache_mount_test.go`, the live dnsdist console
test) and a child process started with its own `TMPDIR` still share the locks
with every other test process. The first implementation derived the directory
on every reservation, so those tests silently locked in a private directory;
the review found this and it is fixed. (Config loading only reads `KUDURRU_*`
variables that contain `__`, and `kudurrutest.Start` strips `KUDURRU_*` from
node processes, so the exported variable reaches no product code.) A candidate is probed for TCP and UDP on 127.0.0.1
before it is handed out, so a port held by a fixed-port service or by a server
that outlived its test is skipped. The kernel drops the locks if a process dies.

Converted callers, names and signatures unchanged so other lanes' new callers
keep compiling: `offlineFreeAddress(t, network)` (cmd/kudurru; used by
`mountedTestConfig`, `offlineConfig` and 17 other test files), `kudurrutest.FreePort`
and through it `NewPorts`, `NewNodeSpec` and `StartUpstream` (all of test/e2e,
test/interop and `internal/rpz` BIND tests), the dnsdist supervisor tests'
`freePort` (`ee/edge/dnsdist`), the doctor fixture config (`doctorConfig`), the
externaldns webhook command test and the dex fixture (`ee/oidc/dex_test.go`).
After review, the remaining close-then-bind sites outside those directories were
converted too: `alternativeAddress` in `internal/dnsserver/altsvc_test.go` (DoH
over TCP and DoH3 over UDP bind the same address), the two QUIC socket group
probes in `internal/dnsserver/quic_socket_test.go`
(`TestQUICSocketPartialBindAndBounds`, `TestQUICSocketConcurrentBind`), the BIND
peer port in `internal/recursor/aggressive_bind_test.go` (opt-in,
`KUDURRU_RECURSOR_BIND=1`) and the AdGuard Home comparator ports in
`internal/blocking/blackbox_test.go` (opt-in; its `unusedPort` helper is gone).
`mountedTestConfig` keeps metrics and internal on `127.0.0.1:0`. Production
listener code is unchanged; no seam was added. `dev-docs/milestones/m5/e2e.md`
describes the allocation.

Callers left as they are, shown not to race: listeners that tests keep open and
pass by address (`net.Listen("127.0.0.1:0")` held for the test, for example the
e2e enrolled relay, the interop notify and relay servers and the doctor "busy"
listeners), and the "nothing listens here" addresses used to provoke refused
connections (`serving_anchor_retained_test.go`, `ee/oidc/e2e_mock_test.go`,
several `internal/*` unit tests). Those never bind the number again (the first
version of this ledger said so of every unconverted site, which the review
showed was false for the four files converted above; a grep for port-picking
helper names across all `_test.go` files found no further one, but sites that
inline the pattern were checked only by the reviewer's reading); at worst
an unrelated socket could answer a refused-connection check, which is not a bind
collision and no failure of that kind has been seen.

Evidence under `.context/v020-final-20261110/evidence/FLAKE-PORT-RACE/`:

- `run02-stress-legacy.log` / `run03-stress-reserve.log` (`stress.sh`): four
  processes at once, each `TestStress` with `-count=2`, 8 goroutines x 125
  subtests that allocate a port, wait 5-24 ms and bind it for TCP and UDP while
  four goroutines per process churn port-0 listeners, UDP sockets and outgoing
  connections. Old helper (listen on 0, close): 466 collisions in 8,000
  allocations (78, 53, 66, 58, 76, 52, 59, 24 per run). `testport`: 0 in 8,000.
- `run12-mounted-procs.log` (`mounted-procs.sh`): four processes of the
  cmd/kudurru test binary at once, each running
  `TestRunServeQuerylogFailedPrepareKeepsOldCompleteRoute` (the hosted
  `bind: address already in use` test), `TestRunServeSQLiteFollowerWaitsWithoutBootstrap`
  and `TestRunServeKubeOfflineSnapshot` with `-count=5` (60 mounted runServe runs),
  alongside a fifth process running `TestStress` (reserve, 4,000 allocations):
  all four PASS, 0 collisions, about 2 minutes. Mounted fixtures cannot run
  concurrently inside one process (`mountedTestConfig` uses `t.Setenv`, which
  `t.Parallel` forbids), so in-process concurrency is exercised by `TestStress`.
- `run01-negative-control-no-flock.log`: with the `flock` call removed,
  `TestReserveExcludesOtherProcesses` fails (`port 22189 reserved by both
  processes`), so that test proves the cross-process exclusion.
- `run04-race-testutil-oidc.log`, `run07-race-dnsdist-rpz.log`: `go test -race`
  of `internal/testutil/...` (kudurrutest, testport), `ee/oidc`,
  `ee/edge/dnsdist` (16 s) and `internal/rpz`: ok.
- `run08-e2e.log` (`e2e.sh`, `KUDURRU_E2E=1`): `TestEnrolledProcessConflictResolution`
  (76 s), `TestRestartKeepsState`, `TestPortConflictsFailFast` (all seven
  subtests; it binds the reserved ports itself and still sees the conflict),
  `TestReadyMeansServing`, `TestStaticClusterProcessesReadOnlyPeerLossRestart`:
  PASS, 129 s.
- `run11-mounted-race.log` (`mounted.sh`): `go vet` of cmd/kudurru, ee/edge/dnsdist,
  ee/oidc, internal/rpz, test/e2e, test/interop, then
  `taskset -c 0-3 go test -race -count=1` of the 92 cmd/kudurru tests that reach
  `mountedTestConfig`, `offlineFreeAddress`, `offlineConfig` or `doctorConfig`
  (selection in `mounted-selection.txt`, computed transitively over the test
  sources) plus `TestExternalDNSWebhook.*`: ok, 1,118 s.
- `run13-lint.log`: scoped golangci-lint v2.15.0 over internal/testutil,
  ee/edge/dnsdist, ee/oidc and cmd/kudurru: 0 issues;
  `run06-lint.log` is the earlier run that found two issues in testport, fixed
  in `test(testport): satisfy the scoped lint`.
- Retained failed attempts: `run05-...-NOT-RUN-permission-denied.log` (script
  not executable), `run09-...-FAILED-vet-unused-import.log` and
  `run10-...-FAILED-build-unused-import.log` (unused `net` imports after the
  conversion, fixed in the amended conversion commit before any test ran).

Review fixes (`fix-*.log`):

- `fix-03-tmpdir-negative-before-fix.log`: the new
  `TestReserveExcludesOtherProcessesAcrossTMPDIR` (child holder started with its
  own `TMPDIR`, parent subtest with `t.Setenv("TMPDIR", ...)`) fails before the
  lock directory fix (`port 22909 reserved by both processes`).
- `fix-04-testport-race-after-fix.log`: `go test -race -count=3` of
  `internal/testutil/testport` and `internal/testutil/kudurrutest` after the fix: ok.
- `fix-01-residual-sites-race.log`: `go test -race` of the converted
  `internal/dnsserver` QUIC socket and Alt-Svc tests, `internal/recursor` and
  `internal/blocking`: ok (`TestAggressiveBIND` and `TestAdGuardBlackBox` skip
  there because they are opt-in). `fix-02-recursor-bind.log`:
  `KUDURRU_RECURSOR_BIND=1` `TestAggressiveBIND` against the installed BIND, all
  8 subtests PASS. No AdGuard Home binary is installed, so the comparator was
  only compiled and vetted.
- `fix-05-mounted-race.log` (the reviewer's mounted selection plus
  `TestRunServeRemoteEdge.*`, `-race -count=2`) and `fix-06-lint.log` (scoped
  lint of the touched packages) were started through the heavy wrapper but had
  not finished when this ledger was written; the integrator should check or
  rerun them.

Mounted fixtures cannot run concurrently inside one process
(`mountedTestConfig` uses `t.Setenv`, which forbids `t.Parallel`), so the
acceptance item "many mounted fixtures concurrently in one process" is met at
the allocator level only (`TestStress`, `TestReserveIsUniqueAndOutsideEphemeralRange`);
mounted fixtures are exercised concurrently only across processes (run12). This
is a documented deviation.

Open: the hosted shard was not rerun on this branch, so the claim that the
hosted `bind: address already in use` failures are gone rests on the mechanism
and the local stress, not on hosted runs. test/interop was vetted but not run
(it needs BIND/Knot containers); its callers go through `kudurrutest.FreePort`.
The refused-connection addresses listed above still use the close-then-dial
pattern. A host whose ephemeral range covers almost all ports would fall back
to the 20000-29999 window, where reservations are still exclusive among test
processes but the kernel could pick a reserved number; `testport.Window()`
reports that case.
