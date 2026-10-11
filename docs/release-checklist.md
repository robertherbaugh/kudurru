# Release checklist

How to cut a kudurru release (first used for v0.1.0-rc.1, then v0.1.0). The
pipeline itself is `.goreleaser.yaml` plus `.github/workflows/release.yml`
(docs/architecture.md §12, docs/deploy/README.md); this file is the order of
the manual steps around it and the checks that need the project owner's
hardware.

Version names: the git tag is `v<version>` (for example `v0.1.0-rc.1`);
goreleaser, the images, the Helm charts and `kudurru version` use the
version without the `v`. Tags with a pre-release suffix (`-rc.1`) publish a
GitHub pre-release and do not move the `latest` or `<major>.<minor>` image
tags (`prerelease: auto`).

## 1. Prepare the release commit

- [ ] `main` is green: `make test` (race, envtest), `make lint`,
      `make check-generated`, `make helm-check` (kubeconform),
      `make web-lint web-test web-build` and `go test ./web/...` against the
      real bundle, `make size`. Do not commit the built `web/dist/index.html`.
- [ ] Every row in docs/followups.md targeted at this release is closed or
      explicitly moved to the next one.
- [ ] Version strings agree:
  - `RELEASE_VERSION` in the Makefile (what a build without a reachable tag
    reports, e.g. from a tarball);
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
- [ ] Pin `gcr.io/distroless/static:nonroot` and `:debug-nonroot` by digest in
      `.goreleaser.yaml` (`base_image`) and the Dockerfile (docs/followups.md).

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
- [ ] `rm -rf dist` afterwards.

Done for v0.1.0-rc.1 in the M5 sandbox with goreleaser 2.18.3: `check` and
the single-target snapshot build succeeded (signing and the full release
skipped: no Docker, no cosign identity).

## 3. Checks on the owner's hardware

These cannot run in CI's sandbox and gate the GA release (an rc may ship
without them, listed as known gaps in the notes):

- [ ] **edge-check** on a Docker runner: `make edge-check` builds the
      kudurru-keepalived image from source and runs `dnsdist --check-config`
      on the 12 golden dnsdist configurations (YAML and Lua) and
      `keepalived -t` on the 4 golden keepalived configurations; the
      dynblocks-ebpf case may need `--cap-add SYS_ADMIN`. Then make the
      `edge` workflow a required check.
- [ ] **kind** (spike f): `kind create cluster`, MetalLB, `helm install
      kudurru-crds` and `kudurru` with `vip.provider=metallb` and
      `service.dns.splitProtocols` both ways; `dig @<lb> example.com` over UDP
      and TCP; `kubectl get dnszones`; rolling restart without dropped
      queries (preStop drain); the edge Deployment with EndpointSlice
      membership.
- [ ] **Two VMs with keepalived 2.4.3** (spike h), compose HA files or the
      chart's keepalived DaemonSet: (1) `kudurru lameduck` on the MASTER keeps
      the VIP and drains the node from dnsdist (D51); (2) `kudurru vip drain
      dns` moves the VIP within the `fall`/down timer; (3) powering off the
      MASTER moves the VIP; record failover times. `kudurru doctor` reports
      `vrrp: OK` (no `--legacy`).
- [ ] dnsdist spikes (a) console directives under YAML and (b) PROXYv2 +
      chashed against a real dnsdist, with `kdig +quic/+tls/+https` through
      the edge.

## 4. Tag and publish

- [ ] `git tag -s v0.1.0-rc.1 -m "kudurru v0.1.0-rc.1"` on the release commit
      and `git push origin v0.1.0-rc.1`.
- [ ] `release.yml` runs: `make test`, `make helm-check`, then `goreleaser
      release --clean`, which publishes the GitHub release with archives,
      deb/rpm/apk, `checksums.txt` (cosign keyless signature and
      certificate), SBOMs, `crds.yaml` and `install.yaml`, and pushes the ko
      images `ghcr.io/robertherbaugh/kudurru:<version>` and `<version>-debug`
      (linux/amd64, linux/arm64). Build provenance is attested for the
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
      same identity flags (confirm `docker_signs` covered the ko images;
      docs/followups.md), and `docker run --rm
      ghcr.io/robertherbaugh/kudurru:<version> version` prints `<version>`.
- [ ] Charts: `helm pull oci://ghcr.io/robertherbaugh/charts/kudurru --version
      <version>` and `cosign verify` on the chart reference; `helm install` on
      the kind cluster from section 3.
- [ ] CRDs: `kubectl apply --server-side -f crds.yaml` from the release assets.
- [ ] Packages: install the deb or rpm on a VM, `systemctl start kudurru`,
      `kudurru doctor`.

## 6. After the release

- [ ] Announce with the CHANGELOG section and the known gaps.
- [ ] For a release candidate: collect issues, fix on `main`, cut `-rc.2` or
      the GA tag. For the GA release: set `RELEASE_VERSION` and the chart
      versions to the next development version (for example `0.2.0-dev`).
