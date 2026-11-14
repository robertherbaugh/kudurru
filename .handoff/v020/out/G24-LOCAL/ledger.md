### G24-LOCAL Local release checks, live recursion and the version-sync CI step — November 13, 2026 (no decision number)

Covers map entries G24-LOCAL, G24-SNAPSHOT, G24-LIVE, G24-SYNCTEST and
G24-TREE. Every check ran in the lane worktree on a clean tree; each log's
first line names the commit. The checks ran on lane commits `f1cf1cac`
(run01) and `59aeee6e` (run02), which are the integrated head `7b11a139`
plus this package's commits only, so their results describe `7b11a139`'s
product code. **They must be repeated on the final release head**, with:

```sh
export DOCKER_HOST=unix:///tmp/kudurru-lab-docker.sock    # only for --full-snapshot
PATH=<dir with syft 1.54.1>:$PATH hack/release-local-checks.sh --full-snapshot --live
```

Lane commits (source only; no generated artifact changed, so there is no
`chore(gen)` commit):

- `1920e4e1` test(recursor): compare the built-in root anchors with IANA's file
- `bd3114b4` ci(go): run the version-sync tests with the other hack/ tests
- `f1cf1cac` build(release): script the local release checks no workflow runs
- `59aeee6e` build(release): check a live recursive node and guard the snapshot disk
- `3709a4e6` test(recursor): check the body close error in the IANA anchor test
- `56443b9a` fix(config): omit an empty strategy so a recursive group boots
- `ed865717` fix(release): find the snapshot ko images under goreleaser.ko.local
- `7acc069b` fix(release): wait for /healthz/ready in the live recursive node check

What now happens. `hack/release-local-checks.sh` runs, on a clean tree, the
checklist steps no pull request workflow runs: `make version-check`, the
version-sync unit tests, `hack/release-tag.sh rc`, `make check-generated`,
`make release-manifests RELEASE_ARTIFACT_VERSION=<VERSION>-rc.<N>` followed by
an inspection that every kudurru image reference in `bin/release` carries the
candidate tag and that `git status` is unchanged, `make dashboard-build`,
`go test -count=1 ./dashboard/...` and `make size` on the real bundle,
`goreleaser check` and the single-target snapshot with `kudurru version`
(goreleaser v2.18.3 built into a scratch directory), optionally the full
snapshot (`release --snapshot --clean --skip=publish,sign`, Docker and syft)
and the live checks. It writes one numbered log per step and a `summary.txt`,
restores the placeholder `dashboard/dist/index.html`, removes `dist/`,
`bin/release` and the dashboard build output, and fails unless
`git status --short` is empty. With `--full-snapshot`,
`hack/snapshot-images.sh` handles the ko images goreleaser loads into the
daemon in snapshot mode, which are named `goreleaser.ko.local:<version>` and
`goreleaser.ko.local:<version>-debug` (goreleaser v2.18.3 `getLocalDomain`;
ko v0.19.1's daemon publisher with `bare: true`): it lists the refs under
that domain before the run and afterwards fails the step unless both images
exist and `kudurru version` in each prints the version from
`dist/metadata.json`, runs `hack/volume-check.sh` on them (reported, not
counted) and removes only the refs the run created. Until `ed865717` the
step looked for `ghcr.io/robertherbaugh/kudurru:*SNAPSHOT*` and would have
skipped all of that on a successful run. The full snapshot's disk guard
(`SNAPSHOT_MIN_FREE_GB`, default 14) is an unverified estimate, stated as such
in the script and the checklist. `hack/live-recursive-node.sh` is the
on-node half of D170: it starts a node with the documented
`default: { mode: recursive, timeout: 4s }`, waits for `/healthz/ready`
(until `7acc069b` it polled `/readyz`, which does not exist, and always waited
60 s) and fails with the node log if readiness never comes. A recursive group
in the configuration file now boots: `UpstreamGroup.Strategy` is
`omitempty`, so the cluster settings seed of a recursive group carries no
`strategy` instead of `""`, which the Kudurru CRD's enum refused. `TestLiveIANATrustAnchors` (gated on
`KUDURRU_LIVE_DNS=1`) fetches IANA's root-anchors.xml over HTTPS and compares
the DS records valid today with `DefaultTrustAnchors`; offline,
`TestIANAAnchorsValidity` checks the validity rule against a trimmed copy of
the file and `TestBuiltInAnchorsAgree` keeps `recursor.DefaultTrustAnchors`
and `dnssec.RootAnchors` identical. In CI, `hack/test_sync_version.py` now runs
in the go job's Test step next to `test_go_test.py` and `test_bench_*.py`; the
helm job's duplicate step (added by QUAL-CI) is removed, and the helm job
keeps `make version-check`.

Evidence under `.context/v020-final-20261110/evidence/G24-LOCAL/`:

- `run01/` (commit `f1cf1cac`, `--full-snapshot --live`; disk 8.5 GB free
  before, 14 GB after): `summary.txt` exit/seconds per step:
  version-check 0 (9 s, PASS); sync-version tests 0 (5 tests OK);
  `hack/release-tag.sh rc` 0 (`version=0.2.0`, `tag=v0.2.0-rc.1`,
  `exists=false`, `previous=v0.1.0`); `make check-generated` 0 (112 s, no
  diff); `make release-manifests RELEASE_ARTIFACT_VERSION=0.2.0-rc.1` 0
  (19 s; `bin/release` has `crds.yaml` 260,056 bytes, `install.yaml` 277,316
  bytes and `compose/` with single, single-edge, ha and the two storage
  overlays); inspection 0: the only kudurru references are
  `ghcr.io/robertherbaugh/kudurru:0.2.0-rc.1` (6 files),
  `kudurru-dnsdist:2.1.3-k0.2.0-rc.1` (4) and
  `kudurru-keepalived:2.4.3-k0.2.0-rc.1` (3); tree unchanged after
  release-manifests 0; `make dashboard-build` 0 (initial load 490.6 KiB of the
  600 KiB budget; embedded bundle 1,085.8 KiB); `go test -count=1
  ./dashboard/...` ok; `make size` 0: **`bin/kudurru: 84832416 bytes, budget
  100000000 bytes (84% used, 14812 KiB headroom)`** with the real bundle
  (linux/amd64, Go 1.27.3; the hosted build job measures the placeholder
  only); goreleaser v2.18.3 install 128 s, `goreleaser check` 0 (1
  configuration file validated); single-target snapshot 0 (17 s):
  `kudurru 0.1.0-rc.5-SNAPSHOT-f1cf1cac (f1cf1cac, go1.27.3, linux/amd64)`,
  84,832,416 bytes, sha256 `f845b7f2…de86ed3b`; live-recursor 0; live
  managed-root 0 (377 s, mostly compiling `./cmd/kudurru`); tree clean 0.
- `run01/15-snapshot-full.log` (retained failure): the full snapshot with
  Docker 25.0.16 (private daemon) and syft 1.54.1 ran the before hooks and
  then failed after 7 min 26 s compiling `darwin_arm64`: `compile: writing
  output: ... no space left on device`. No archive, package, SBOM or image was
  produced; this run filled the shared disk for a few minutes.
- `run02/` (commit `59aeee6e`, `--full-snapshot --live --skip-generated`;
  disk 10 GB free before, 6.7 GB after): every step as in run01 with the same
  results (`make size` 84,832,416 bytes again; single-target snapshot
  `kudurru 0.1.0-rc.5-SNAPSHOT-59aeee6e`, sha256 `a6368c34…ad5ea5fb`), except
  `14-snapshot-full.log`, exit 1 by design: "only 6 GB free here; the full
  snapshot needs about 12 GB" (the new guard, so the shared disk was not
  filled again). `17-live-recursive-node.log`: all six checks ok.
- Live internet (outbound UDP/TCP 53 and HTTPS work in this sandbox):
  `01-live-internet.log` `TestLiveInternet` PASS (0.70 s) at `7b11a139`;
  `02-live-anchors.log` `-race` `TestLiveInternet`, `TestLiveIANATrustAnchors`,
  `TestIANAAnchorsValidity`, `TestBuiltInAnchorsAgree` 4 PASS (2.8 s): IANA's
  file on November 13, 2026 at 10:40 UTC lists as valid exactly
  `20326 8 2 E06D44B8…7C7F8EC8D` and `38696 8 2 683D2D0A…C0FB2B16` (KSK-2010
  19036 expired 2019-01-11), equal to both built-in copies. The comparison
  trusts HTTPS from data.iana.org; it does not verify the file's CMS
  signature. `run01/17-live-managed-root.log` `TestLiveRootManagedBootstrap`
  PASS: all 13 IPv4 root addresses answered with KSKs 20326 and 38696, signed
  by 38696; seed 38696 authenticated the DNSKEY set on 13 of 13, 20326 on 0 of
  13 (published, not signing), matching the November 13 line in the checklist.
  `08-live-recursive-node.log` and `run02/17-live-recursive-node.log`
  (`kudurru 0.2.0` built at `f1cf1cac`/`59aeee6e`): `dig +dnssec example.com`
  NOERROR with `ad`; `dnssec-failed.org` SERVFAIL with `EDE: 6 (DNSSEC
  Bogus)`; `+cd` NOERROR with the A record; `kudurru_recursor_outgoing_queries_total`
  ok 18, timeout 0, error 0.
- `07-recursive-group-doc-config.log` (**retained failure, product bug, fixed
  in `56443b9a`**):
  the documented file configuration `default: { mode: recursive, timeout: 4s }`
  (docs/guide/admin/07-zones-and-upstreams.md:454) does not start a first-boot
  SQLite node at `f1cf1cac`: `error: initialize runtime owners:
  Kudurru.kudurru.io "default" is invalid:
  [spec.settings.upstreams.groups.default.strategy: Unsupported value: "":
  supported values: "parallel_best", "strict", "random", "rtt_weighted"]`.
  `config.Upstreams.normalize` leaves `Strategy` empty for recursive groups,
  `UpstreamGroup.Strategy` is serialized without `omitempty`, and the Kudurru
  CRD enum refuses `""`. No mounted test starts a node with a recursive group
  from the configuration file. The live node script set
  `strategy: parallel_best` to work around it until `7acc069b`.
- `03-sync-version-tests.log` 5 tests OK; `04-sync-version-tests-no-helm.log`
  the same 5 OK with neither helm nor kustomize on `PATH` (so the go job's
  runner is enough); `05-deploy-release-tests.log` `TestGoreleaserConfig`,
  `TestVersionFile`, `TestVersionSyncScript`, `TestReleaseTagScript`,
  `TestWorkflowActionsPinned` PASS at `bd3114b4`; `ci.yml` parses as YAML (14
  jobs).
- `06-tree.log` (G24-TREE): the main checkout at `7b11a139` shows only
  ` M dashboard/dist/index.html`, nothing staged; the working copy is a built
  index (0 placeholder markers, 23 `./assets/` references, +34/-3 lines);
  `HEAD:dashboard/dist/index.html` has 1 `kudurru:placeholder` marker. The
  lane worktree is clean and every script run ended with `git status --short`
  empty. The main checkout's file was not touched (the brief forbids editing
  it); restoring it is `git checkout -- dashboard/dist/index.html` there.
- `09-bench-guards.log` 14 bench guard tests OK; `10-lint.log` (retained):
  golangci-lint v2.15.0 on `./internal/recursor/...` found 1 errcheck issue
  (`defer resp.Body.Close()`), fixed in `3709a4e6`; `11-lint-race.log` after the fix: 0 issues, `go vet` ok, `KUDURRU_LIVE_DNS=1 go test -race ./internal/recursor` ok (7.2 s).

- Review fixes (lane head `7acc069b`): `fix-01-config-seed-before.log`
  `TestRecursiveGroupSeedOmitsStrategy` FAIL without the fix (seed has
  `"strategy":""`); `fix-02-config-seed-after.log` it and
  `TestRecursiveGroupReplacesDefault` PASS. `fix-03-mounted-before.log`
  `TestRunServeRecursiveGroupFromFileFirstBoot` FAIL without the fix (0.40 s,
  the CRD enum error); `fix-04-mounted-after.log` `-race` it (5.7 s),
  `TestRecursiveNTAReloadAndSettingsRestore`,
  `TestRunServeDirectAnchorsFencedRecursiveValidation` (15.9 s) and
  `TestRunServeHumanHealthDetails` 4 PASS. `fix-09-config-race.log` `-race`
  `./internal/config`, `./internal/upstream`, `./internal/importers` ok;
  `fix-10-api-settings.log` `./internal/api -run 'Settings|OpenAPI|Upstream|Recursi'`
  ok; `fix-12-cmd-settings.log` `./cmd/kudurru -run
  'Reload|Settings|Upstream|Recursive|ClusterSeed'` ok (103 s);
  `fix-17-lint.log` golangci-lint on `./internal/config/...` 0 issues.
  `fix-05-snapshot-images-tests.log` `hack/test_snapshot_images.py` 7 tests
  OK (fake docker: both images found and version-checked, removal limited to
  the refs the run created, missing images and a wrong version fail);
  `fix-06-snapshot-images-old-filter.log` the old filter matches neither
  goreleaser.ko.local ref, and `snapshot-images.sh list`/`check` against the
  private daemon list nothing and fail as expected without touching it.
  `fix-08-live-node-old-binary.log`: the script with the documented
  configuration on the old binary (`59aeee6e`) exits 1 in 1.6 s and prints
  the enum error; `fix-14-live-node.log` on `bin/kudurru` built at
  `56443b9a`: ready in about a second, 6 of 6 checks ok, 18 outgoing queries,
  0 timeouts, 6.8 s total (65 s before); `fix-15-live-node-never-ready.log` a
  fake binary that never serves: exit 1 after 60 s with "the node was not
  ready". `fix-13-deploy-release-tests.log` the five deploy release tests
  PASS with the new ci.yml line; `fix-16-bench-guards.log` 14 OK.

What remains open. The full goreleaser snapshot (archives, deb/rpm/apk,
SBOMs, checksums, both ko images with `kudurru version`, `hack/volume-check.sh`
on them) has **not** succeeded: Docker and syft were available, but the
shared disk (8.5 GB, then 6 to 10 GB free) is too small for four
cross-compiled builds; it needs a machine with an estimated 14 GB free (a
guess: 8.5 GB was not enough, nothing more is known), or a label-triggered
hosted dry-run workflow (map option B, not built here). The image half
(`hack/snapshot-images.sh`) is tested only against a fake docker and has
never run on real snapshot images; the first full run proves it. Record
G24-SNAPSHOT as an open gap, not passed. Even
then, a local snapshot uses the plain distroless bases (publish.yml builds
`images/base` first), so `volume-check` on the snapshot images is expected to
fail on the missing directories; the hosted edge workflow's `make
volume-check` covers the image layout. Signing is not exercised locally. The
snapshot version is `0.1.0-rc.5-SNAPSHOT-<commit>` because the nearest
reachable tag on this branch is `v0.1.0-rc.5`; the real candidate's
manifests use `0.2.0-rc.1`, as the release-manifests step shows. The recursive
group fix changes only the JSON of a recursive group (no `strategy` key); the
integrator's full `./cmd/kudurru` run has not covered it yet. The
G24-SYNCTEST step (and the new `test_snapshot_images.py` line) still has to appear and pass in the hosted ci run on the
final head. Every result here must be repeated on the final head.
