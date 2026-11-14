# QUAL-CI: bullets for `CHANGELOG.md` `[0.2.0]`

One bullet below is a product change (the first under Fixed). Everything else
changes tests, harnesses and workflows only; those bullets are for
contributors and for operators who run the acceptance harnesses themselves.
Drop them if the changelog should stay product-only.

### Changed

- `make test` hands the envtest binaries that `make envtest` installed to every
  Kubernetes test suite by exporting `KUBEBUILDER_ASSETS` when it is unset.
  Five packages read only that variable and silently skipped their Kubernetes
  tests without it, including in hosted CI.
- The acceptance harnesses can run on built artifacts and say what they ran:
  `KUDURRU_E2E_LICENSED_BINARY` names a prebuilt `-tags licensetest` binary for
  the licensed multi-node end-to-end tests (next to the existing
  `KUDURRU_E2E_BINARY`), `hack/kind-e2e.sh` accepts `BUILD=0 IMAGE=<repository:tag>`
  as `hack/compose-e2e.sh` does, and each harness logs the version, commit and
  checksum of the binary or image under test. `hack/kind-e2e.sh` refuses, before
  it creates a cluster, an image reference that is a digest or has no tag, a
  prebuilt image that the local Docker daemon does not have, and an image that
  does not accept the test license key.
- New `make interop-internal` target for the BIND 9 interoperability tests of
  response policy zones, BIND-signed secondary zones and stub zones.
  `KUDURRU_REQUIRE_BIND=1` and `KUDURRU_REQUIRE_ENVTEST=1` make a test fail
  instead of skip when BIND 9 or the envtest control plane is missing, as
  `KUDURRU_REQUIRE_POSTGRES=1` already does for PostgreSQL.

### Fixed

- A query whose upstream fails or times out is answered with SERVFAIL also
  when a configuration change is committed around it. Until the change was
  being served, the node sent such a query no answer at all, and the client
  waited for its own timeout before it tried another resolver; forwarded
  answers were not affected. (Product change; needs the decision row in
  `out/QUAL-CI/decision.md`.)
- CI runs sixteen acceptance tests that skipped in every earlier hosted run:
  five BIND 9 interoperability tests in internal packages, the two-node
  central audit and query log tests against PostgreSQL, the three-replica
  Kubernetes audit test, and eight Kubernetes tests (Lease handoff, cold start
  from `local.db`, the Kubernetes store driver, zone deletion and Kubernetes
  identities) that never received the envtest binaries. (Add this bullet only
  after the first hosted run with these commits is green; nothing here ran on
  GitHub Actions yet.)
- The release workflow installs the envtest binaries before its test job. The
  primary-zone runtime tests added in 0.2.0 fail without them, which would have
  stopped the first 0.2.0 candidate before publishing.

### Not a changelog entry

- The unit tests of the version synchronization script are not newly run. They
  already ran in every hosted `go` job and in the release workflow's test job
  through the Go test `TestVersionSyncScript` (package `deploy`). The step this
  package added to the `helm` job runs the same five tests a second time so
  that their result has a line of its own in the log. The earlier draft of this
  file listed it under Fixed as newly run; that was wrong.
