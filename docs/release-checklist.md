# Release checklist

How to cut a kudurru release (written for v0.1.0-rc.1, then v0.1.0; no tag
exists yet). The pipeline is the branch workflows below plus
`.goreleaser.yaml` and `.github/workflows/publish.yml` (docs/architecture.md
§12 and D139, docs/deploy/README.md); this file is the order of the manual
steps around it and the checks that need the project owner's hardware.

Version names: `VERSION` in the repository root holds `MAJOR.MINOR.PATCH`
(no `v`, no suffix) and is the only version maintained by hand. The git
tags are `v<version>-rc.<N>` for release candidates and `v<version>` for the
release; goreleaser, the images, the Helm charts and `kudurru version` use
the version without the `v`. A release candidate publishes a GitHub
pre-release and does not move the `latest` image tags (`prerelease: auto`).

## Release pipeline

```
feature ──PR──▶ main ──PR──▶ release-candidate ──PR──▶ release
           dev-<PR>         v<VERSION>-rc.<N>           v<VERSION>
           image            pre-release, image         release, image + latest
```

| Step | Workflow | What it publishes |
|---|---|---|
| Push to a pull request against `main` (same repository, not Dependabot) | `dev-image.yml` | `ghcr.io/robertherbaugh/kudurru:dev-<PR number>` from the PR's head commit with the dashboard bundle, linux/amd64 and linux/arm64, unsigned. Every push overwrites the same tag; `kudurru version` reports `<VERSION>-dev.<PR>` and the commit. |
| Push to `release-candidate` (a merge from `main`) | `release-candidate.yml` | Tags `v<VERSION>-rc.<N>` (N is one more than the highest candidate of that VERSION), then `publish.yml`: the GitHub pre-release, images `<VERSION>-rc.<N>` and `<VERSION>-rc.<N>-debug`, charts and supervisor images at that version. |
| Push to `release` (a merge from `release-candidate`) | `release.yml` | Tags `v<VERSION>`, then `publish.yml`: the GitHub release, images `<VERSION>`, `latest`, `<VERSION>-debug` and `latest-debug`, charts and supervisor images, then `docs-pages.yml` publishes the docs site from the tag. |
| Pull request into `release-candidate` or `release` | `promotion.yml` | Nothing: fails unless the source branch is `main` (for `release-candidate`) or `release-candidate` (for `release`), and dry-runs the tag the merge would create. |

`hack/release-tag.sh rc|release` computes the tags and refuses:

- a VERSION that is not `MAJOR.MINOR.PATCH`;
- a candidate or release for a VERSION whose `v<VERSION>` tag already
  exists: bump `VERSION` on `main` after every release;
- a release with no `v<VERSION>-rc.<N>`, or whose tree differs from the
  latest candidate's: a release ships exactly what its last candidate
  shipped. If `release-candidate` was pushed again while the promotion PR
  was open, wait for the new candidate to be tagged, then re-run the
  `promotion` check (or the `release` workflow).

Re-running a workflow at the same commit reuses its tag. To re-publish an
existing tag by hand, run `publish` with workflow_dispatch and the tag.

The workflows push the tags with `GITHUB_TOKEN`; such tags start no other
workflow, which is why the branch workflows call `publish.yml` and
`docs-pages.yml` themselves. Pushing a `v*` tag by hand publishes nothing.

Repository settings the pipeline relies on (once):

- [ ] Branches `release-candidate` and `release` exist (created from `main`
      before the first promotion; creating a branch publishes nothing) and
      are protected: changes through pull requests only, `promotion` and the
      `ci` checks required.
- [ ] Promotion pull requests are merged with a merge commit, not squash or
      rebase (a branch ruleset can restrict the merge method per branch):
      `release-candidate` must keep `main`'s history, or the next promotion
      conflicts and the release tree check fails.
- [ ] Tag rulesets, if any, let GitHub Actions create `v*` tags.
- [ ] A branch ruleset refuses branches named `v*`. publish.yml checks out
      `refs/tags/<tag>`, so a branch named like the next tag can no longer
      stand in for it, but it would still confuse anyone checking out the
      bare name.
- [ ] The `github-pages` environment allows deployments from the `release`
      branch (docs-pages runs in the context of the `release` push).
- [ ] The `ghcr.io/robertherbaugh/kudurru` package grants this repository
      write access (it does when the first image was pushed by a workflow
      of this repository).

## 1. Prepare the release commit

- [ ] `main` is green: `make test` (race, envtest), `make lint`,
      `make check-generated`, `make helm-check` (kubeconform),
      `make web-lint web-test web-build` and `go test ./web/...` against the
      real bundle, `make size`. Do not commit the built `web/dist/index.html`.
- [ ] Every row in docs/followups.md targeted at this release is closed or
      explicitly moved to the next one.
- [ ] Version strings agree:
  - `VERSION` holds the version being released (the Makefile's
    `RELEASE_VERSION`, what a build without a reachable tag reports, reads
    it);
  - `version` and `appVersion` in `charts/kudurru/Chart.yaml` and
    `charts/kudurru-crds/Chart.yaml` (the release workflow re-stamps both
    from the tag when packaging, the committed values are what `helm
    install ./charts/kudurru` from a checkout uses);
  - after changing the charts: `make deploy-assets` (the kustomize bases
    carry `app.kubernetes.io/version`) and `make rbac-manifests` if
    `internal/rbac/permissions.yaml` changed;
  - the image tags in `deploy/compose/**/docker-compose.yml`
    (`kudurru:<version>`, `kudurru-dnsdist:<dnsdist>-k<version>`,
    `kudurru-keepalived:<keepalived>-k<version>`); for a release candidate
    keep the GA tags and say so in the release notes.
- [ ] CHANGELOG.md: the section for the version, with the date on the GA
      release.
- [ ] The distroless bases are pinned by digest (`tag@sha256:<index digest>`)
      in `.goreleaser.yaml` (`base_image`) and the Dockerfile, the same
      digests in both (`TestDockerfile`). To bump: `curl -sI -H 'Accept:
      application/vnd.oci.image.index.v1+json'
      https://gcr.io/v2/distroless/static/manifests/nonroot` (and
      `debug-nonroot`), take `docker-content-digest`.

## 2. Dry-run the pipeline locally

goreleaser v2 (a downloaded release binary, checksum verified, or
`go run github.com/goreleaser/goreleaser/v2@latest`):

- [ ] `goreleaser check` validates `.goreleaser.yaml`.
- [ ] `goreleaser build --snapshot --single-target --clean`, then
      `./dist/kudurru_linux_amd64_v1/kudurru version`. Without a tag the
      snapshot is `0.0.0-SNAPSHOT-<commit>`; the before hook also runs
      `make release-manifests` (kustomize into `bin/release/`).
- [ ] With Docker available: `goreleaser release --snapshot --clean
      --skip=publish,sign` (archives, deb/rpm/apk, SBOMs, ko images into the
      local daemon).
- [ ] The before hooks run `make web-build`, so every goreleaser run needs
      Bun on `PATH`; `git checkout web/dist/index.html` afterwards restores
      the committed placeholder.
- [ ] `rm -rf dist` afterwards.

Dry runs for v0.1.0-rc.1 (not tagged yet):

- M5 sandbox, goreleaser 2.18.3: `check` and the single-target snapshot
  build succeeded (no Docker, no cosign identity).
- v0.1 fix round (2026-10-11, at a57eb73 plus the digest pin of 82f9b8e, Docker 25, goreleaser 2.18.3
  and syft 1.54.1, both checksum verified): `goreleaser check` validated the
  configuration; `goreleaser release --snapshot --clean --skip=publish,sign`
  succeeded in 39 s with a warm cache (5 min cold): 5 archives (linux and
  darwin amd64/arm64, compose), deb/rpm/apk for amd64 and arm64, SBOMs,
  checksums, and both ko images loaded into the daemon on the pinned
  distroless bases (`kudurru version` in each printed
  `0.0.0-SNAPSHOT-<commit>`). syft must be on `PATH` for the SBOM step.
  `dist/` removed afterwards. Signing is not exercised locally (keyless
  cosign needs the GitHub OIDC token).

## 3. Checks before GA (CI and the owner's hardware)

These gate the GA release (an rc may ship without them, listed as known
gaps in the notes).

Two former items here run on GitHub's ubuntu-latest runners now:

- [ ] **edge-check** (`edge` workflow, no `continue-on-error` since the v0.1
      fix round): `make edge-check` builds the kudurru image and both
      supervisor images (keepalived from source) and runs `dnsdist
      --check-config` on the 12 golden dnsdist files (the dynblocks-ebpf
      case as root with `--cap-add SYS_ADMIN`) and `keepalived -t` on the 4
      golden keepalived files. It passed locally from a clean clone with no
      build cache at a57eb73. Make the `edge` workflow a required check on
      `main` once it is green there.
- [ ] **kind** (spike f, `kind` workflow, `make kind-e2e`): kind cluster,
      MetalLB L2, `helm install kudurru-crds` and `kudurru` with
      `vip.provider=metallb` and `service.dns.splitProtocols` both ways;
      `dig` an authoritative and a forwarded name through the LoadBalancer
      over UDP and TCP; `kubectl get dnszones`; a rolling restart with no
      unanswered query. Not yet run anywhere (the fix-round sandbox cannot
      start kind); make it required after green runs. Still manual: the
      edge Deployment with EndpointSlice membership.

On the owner's hardware:

- [ ] **Two VMs with keepalived 2.4.3** (spike h), compose HA files or the
      chart's keepalived DaemonSet: (1) `kudurru lameduck` on the MASTER keeps
      the VIP and drains the node from dnsdist (D51); (2) `kudurru vip drain
      dns` moves the VIP within the `fall`/down timer; (3) powering off the
      MASTER moves the VIP; record failover times. `kudurru doctor` reports
      `vrrp: OK` (no `--legacy`).
- [ ] dnsdist spikes (a) console directives under YAML and (b) PROXYv2 +
      chashed against a real dnsdist, with `kdig +quic/+tls/+https` through
      the edge.

## 4. Promote and publish

- [ ] Release candidate: open a pull request from `main` into
      `release-candidate`, wait for `promotion` and CI, merge it with a merge
      commit. `release-candidate.yml` tags `v<VERSION>-rc.<N>`.
- [ ] Release: once the candidate passed section 3, open a pull request from
      `release-candidate` into `release` and merge it with a merge commit.
      `release.yml` tags `v<VERSION>`.
- [ ] `publish.yml` runs for the tag: `make test`, `make helm-check`, then
      `goreleaser release --clean` (its before hook builds the dashboard
      bundle into web/dist), which publishes the GitHub release with archives,
      deb/rpm/apk, `checksums.txt` (cosign keyless signature and
      certificate), SBOMs, `crds.yaml` and `install.yaml`, and pushes the ko
      images `ghcr.io/robertherbaugh/kudurru:<version>` and `<version>-debug`
      (linux/amd64, linux/arm64), plus `latest` and `latest-debug` for a
      release. Build provenance is attested for the
      archives, packages and checksums.
- [ ] The workflow packages both charts with `--version <version>
      --app-version <version>`, pushes them to
      `oci://ghcr.io/robertherbaugh/charts` and cosign-signs each chart digest.
- [ ] The `supervisor-images` job (after `release`) builds and pushes
      `ghcr.io/robertherbaugh/kudurru-dnsdist:2.1.2-k<version>` and
      `ghcr.io/robertherbaugh/kudurru-keepalived:2.4.3-k<version>` from
      `images/dnsdist` and `images/keepalived` with
      `KUDURRU_IMAGE=ghcr.io/robertherbaugh/kudurru:<version>` for
      linux/amd64 and linux/arm64 and cosign-signs each digest (the program
      versions come from the Dockerfile ARGs). Check both jobs succeeded;
      the image READMEs carry the GPL notices and source offer.

## 5. Verify what was published

- [ ] Checksums: `cosign verify-blob --certificate checksums.txt.pem
      --signature checksums.txt.sig --certificate-identity-regexp
      'https://github.com/robertherbaugh/kudurru/' --certificate-oidc-issuer
      https://token.actions.githubusercontent.com checksums.txt`, then
      `sha256sum -c checksums.txt` on the downloaded artifacts.
- [ ] Images: `cosign verify ghcr.io/robertherbaugh/kudurru:<version>` with the
      same identity flags (`publish.yml` already ran this for both ko
      images: `docker_signs` with `artifacts: manifests` signs the ko index
      digests), and `docker run --rm
      ghcr.io/robertherbaugh/kudurru:<version> version` prints `<version>`.
- [ ] Charts: `helm pull oci://ghcr.io/robertherbaugh/charts/kudurru --version
      <version>` and `cosign verify` on the chart reference; `helm install` on
      the kind cluster from section 3.
- [ ] CRDs: `kubectl apply --server-side -f crds.yaml` from the release assets.
- [ ] Packages: install the deb or rpm on a VM, `systemctl start kudurru`,
      `kudurru doctor`.

## 6. After the release

- [ ] Announce with the CHANGELOG section and the known gaps.
- [ ] For a release candidate: collect issues, fix on `main`, merge `main`
      into `release-candidate` again for the next `-rc.<N>`, or promote the
      candidate to `release`. For the release: bump `VERSION` on `main` to
      the next version (for example `0.2.0`) and the chart versions with it;
      the next promotion into `release-candidate` fails until `VERSION` is
      bumped.
