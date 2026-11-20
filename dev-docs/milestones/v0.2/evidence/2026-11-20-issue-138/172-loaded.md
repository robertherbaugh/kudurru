# #172 — Loaded mounted verification

November 20, 2026. Synthetic local VM (Linux x86_64, eight logical CPUs,
Go 1.27.4), base `ce3ab6e2` with test-only attribution. Kubernetes used
envtest 1.37.0 from `make envtest`. This is not the owner's loaded lab or
hosted CI. The original v0.2 audit logs are not in this checkout.

## Non-race observations

| Profile | Long apply | Static twins / restart | Refusal walks (standalone, static, Kubernetes) | Status writes |
|---|---|---|---|---|
| [0 loops, 1 P](172-norace-mounted-p1-load0-package-cwd.log) | 0 failed reads, 267 ms DNS gap | Pass | 3/3 (212 operations each) | 24 writes, no silent probes or SERVFAIL |
| [1 loop, 1 P](172-norace-mounted-p1-load1-package-cwd.log) | **Fail:** 3 reads failed, final `generation_pending`; 366 ms gap | Pass | 3/3 (212 operations each) | 24 writes, no silent probes or SERVFAIL |
| [2 loops, 2 Ps](172-norace-mounted-p2-load2-package-cwd.log) | **Fail:** 11 reads failed, final `generation_pending`; 306 ms gap | Pass | 3/3 (212 operations each) | 24 writes, no silent probes or SERVFAIL |

The long apply returned all 120 creates in each profile, and no DNS probe
saw SERVFAIL. The failing reads received a product refusal, not an HTTP
client timeout. The [first 17 completions](172-norace-long-rates-checkpoint.log)
of a separate one-core, zero-loop `-count=100` series had one failing sample
(one in-window `generation_pending`), zero SERVFAIL, and no one-second DNS
gap breach (longest 356 ms). Only count read failures within the logged
apply window in these early runs; the early diagnostic's last failed read
could be outside that window. The final test code ties it to the window.

The [first zero-loop invocation](172-norace-mounted-p1-load0.log) and the
[interrupted one-loop invocation](172-norace-mounted-p1-load1.log) ran the
compiled binary from the repo root. Kubernetes could not locate its relative
CRD path, so those entries are setup failures before observation, not product
refusals. The logs also retain the observed read failure in the one-loop run.
The corrected profiles run in `cmd/kudurru`. A [non-race smoke run after
attribution refinements](172-final-smoke.log) passed long apply and both
readiness unit tests.

## Race observations

- [Zero loops, one P](172-race-mounted-p1-load0.log): all six mounted cases
  passed. Long apply had a 975 ms maximum DNS gap, zero SERVFAIL, and 52
  failed reads alongside 11 successful reads; the last refusal was
  `generation_pending`. The race variant requires some successful reads,
  not zero failed reads. Status-write and refusal-walk counts are retained
  in the log.
- [Two-P readiness credit](172-race-readiness-credit.log): with one pinned
  CPU, two busy loops, and the race test at `nice -n 7`, node A became ready at
  72.734 s, after 598 scheduler-credit checks. Node B failed at the unchanged
  two-minute wall ceiling after 509 credit checks. Its final readiness probe
  timed out with no HTTP status. The fixture failed **before** the login
  requests, not because the product returned a refusal. This exercised the
  mounted multi-P budget without treating the run as a pass.

## Reproduction

```sh
make envtest
GOMAXPROCS=2 go test -p 2 -c -o .context/p3/kudurru.test ./cmd/kudurru
GOMAXPROCS=2 go test -p 2 -race -c -o .context/p3/kudurru-race.test ./cmd/kudurru
export KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" KUDURRU_REQUIRE_ENVTEST=1
cd cmd/kudurru
pattern='^Test(RunServe(SQLiteLongApplyKeepsServingDNSAndReads|SQLitePrimaryAnswersAcrossStatusWrites|StaticTwinsReadOnlyPeerLossRestart|RefusalInventory(Standalone|Static|Kubernetes))|ReadinessBudgetChargesOnlyRunnableTime|ProcessSchedStatReadsThisProcess)$'
GOMAXPROCS=1 taskset -c 0 ../../.context/p3/kudurru.test -test.v -test.run="$pattern" -test.count=1
GOMAXPROCS=1 taskset -c 1 ../../.context/p3/kudurru.test -test.v -test.run='^TestRunServeSQLiteLongApplyKeepsServingDNSAndReads$' -test.count=100
```

For loaded profiles, start one or two `taskset -c 0 bash -c 'while :; do :;
done'` workers and stop them when the test exits. The two-loop case sets
`GOMAXPROCS=2` while still pinned to one core. The race profiles use the
race binary and CPU 2 for both the test and workers. The logs record the
test exit, `nproc`, initial load, and per-case scheduler measurements.
For the dedicated readiness-credit case, recompile the final race test,
pin it to CPU 0 with two workers, and use `GOMAXPROCS=2 taskset -c 0 nice -n 7`
with `-test.run='^TestRunServeStaticTwinsReadOnlyPeerLossRestart$'`.

## Still open

- Finish recording the one- and two-loop race profiles and the full
  100-sample rate. The synthetic VM does not establish an owner-lab rate.
- The original 1.39 s DNS gap is not diagnosed. The older five-minute
  refusal-walk timeout remains an attributed failure; the current walk
  still uses its original five-minute context. The historical status-write
  mounted/lint evidence remains unavailable; the fresh mounted probes above
  do not substitute for the unavailable lint log.
