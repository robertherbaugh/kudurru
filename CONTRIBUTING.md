# Contributing to kudurru

Thank you for helping build kudurru. This file covers the rules that are easy to get wrong; how the code fits together is in the [architecture tour](docs/architecture/README.md), and the design and its decision log are in the [design record](dev-docs/design-record.md).

## Licensing rule (read this first)

The core uses the [Kudurru Community License](LICENSE); enterprise code and documentation have their own notices in [ee/LICENSE.md](ee/LICENSE.md) and [docs-site/LICENSE.md](docs-site/LICENSE.md). Isolated development and contributions are permitted under the community terms, including for businesses and governments. Changes to the website and license server require prior written authorization and separate contribution terms under [website/LICENSE.md](website/LICENSE.md) and [license-server/LICENSE.md](license-server/LICENSE.md).

Because the project is not open source and its owner also offers commercial licenses, no GPL, EUPL or other copyleft code can ever be part of it. Several of the projects it learns from are **GPL or EUPL**: Gravity (GPL-3.0), Technitium (GPL-3.0), AdGuard Home and urlfilter (GPL-3.0), Pi-hole (EUPL-1.2), dnsdist and keepalived (GPL-2.0). Code, markup, and data from those projects must never be copied, translated, or "ported" into this repository. Reading their docs and describing their behavior is fine; the research notes under `dev-docs/research/` do exactly that. Reimplement from the specification or from a permissively licensed reference (blocky, dnsproxy, CoreDNS, external-dns; see [NOTICE](NOTICE)).

dnsdist and keepalived are run as separate programs in their own images under `images/`. They are never linked, vendored, or patched.

CI runs `go-licenses` with a deny list for GPL and EUPL modules. Embedded data (the starter blocklist) must come from permissively licensed sources with a provenance file.

## Contributor agreement

For community contributions to the core, enterprise materials and documentation, accept the following contributor agreement separately by stating "I accept the contributor agreement in CONTRIBUTING.md" in your pull request:

You grant Robert Herbaugh, the project owner, a perpetual, worldwide,
non-exclusive, royalty-free, irrevocable license to use, reproduce, modify,
sublicense and distribute your contribution under any terms, including
community, commercial and proprietary terms. You keep your copyright.
Submitting a pull request alone does not transfer ownership or grant these rights.

Every commit must also carry a `Signed-off-by: Name <email>` line
(`git commit -s`), certifying the
[Developer Certificate of Origin 1.1](https://developercertificate.org/).
The DCO sign-off does not replace acceptance of the contributor agreement.
Pull requests without both are not merged.

## Ownership and review

Changes to the license server, website, licensing terms and
enforcement, and branding require review from `@robertherbaugh` under
[CODEOWNERS](.github/CODEOWNERS). The ownership rules also cover CI and
release publishing. The repository's branch rules require code-owner
approval on `main` and the release branches; a CODEOWNERS file must be
present on the pull request's base branch for GitHub to apply it.

Original artwork, exported assets and color tokens live in
[branding/](branding/README.md). Update that source and regenerate app
copies with the documented export scripts.

## Building

```sh
make build        # bin/kudurru
make test         # unit tests with -race
make lint         # golangci-lint, go vet, generated-file drift
make generate     # controller-gen: deepcopy, CRDs, RBAC
make sast         # Semgrep (Docker); make vulncheck, make trivy-fs, make trivy-images
```

The security scans (`.github/workflows/security.yml`, and publish.yml's scan of a release candidate's pushed images, `make trivy-published`) fail on a finding. Fix it, or, for a false positive, add a `// nosemgrep: <rule> -- <reason>` comment above the line or an entry with its reason to `.trivyignore.yaml`.

Go 1.27 is required. `CGO_ENABLED=0` is a release requirement; do not add cgo dependencies.

## Branches and releases

Open pull requests against `main`; each push to one builds `ghcr.io/robertherbaugh/kudurru:dev-<PR number>` for trying the branch. Releases are cut by merging `main` into `release-candidate` (tags `v<VERSION>-rc.<N>`, a GitHub pre-release) and `release-candidate` into `release` (tags `v<VERSION>`, moves `latest`), where `VERSION` is the file in the repository root. Never push release tags by hand. The steps and the checks around them are in [dev-docs/release-checklist.md](dev-docs/release-checklist.md#release-pipeline).

After editing `VERSION`, run `make version-sync`. To set it and synchronize in
one command, run `python3 hack/sync-version.py --version 0.3.0` (substitute the
next version). `make version-check` reports drift without writing; CI runs it.
These commands need Python 3.9+, Git, Go and Helm on `PATH` in a Git checkout.

The script updates both charts, supervisor version defaults from their
Dockerfiles, generated Kustomize manifests, Compose images, and current install
references in the README and deployment/start guides. It adds a missing
changelog section marked unreleased. Release notes, dates, feature claims and
historical links still need editorial review; API/CRD versions, dependency
versions, digest pins and development tags have independent lifecycles.
Rendering finishes in a temporary directory before tracked files change.

GoReleaser stages manifests and the Compose bundle with the exact GA or RC tag
in `bin/release`, preserving tracked source. To inspect an RC bundle locally,
run `make release-manifests RELEASE_ARTIFACT_VERSION=0.3.0-rc.1` with a base
matching `VERSION`; this installs pinned kustomize if needed and publishes
nothing. Only tracked deployment files enter the bundle, excluding local
Compose secrets and data.

## Layout and seams

- `api/v1alpha1/` holds the Go types for every kind. The same structs are the CRDs, what the SQLite driver persists, and what the REST API, CLI and UI exchange. Change the type, run `make generate`, and all three follow.
- `internal/store` is the persistence seam. Serving code never reads the store on the query path; it reads snapshots materialised by `zonecache`.
- `internal/pipeline` defines the fixed stage chain. Stage order is code, not configuration.
- `ee/integration` is the only way to add outbound sinks (Cloudflare) or inbound sources (UniFi). Integrations never run on the query path.
- `internal/rbac/permissions.yaml` is the single source for roles, Kubernetes ClusterRoles and the docs table. Every API route declares its permission; a lint fails the build otherwise.
- `internal/config/schema.go` describes every settings key. Keys flagged `hostOnly` can never be set through the API.

Deferred features have seams documented in [design-record.md §13.1](dev-docs/design-record.md#131-v010--stone). Land a deferred feature by filling its seam, not by adding a parallel path.

## Decisions

Architecture changes go through the decision log in [design-record.md §16](dev-docs/design-record.md#16-decision-log): add a row with the topic, the decision, why, and the alternatives rejected. Open questions live in [dev-docs/open-questions.md](dev-docs/open-questions.md).

## Commit messages

Conventional commits (`feat(blocking): ...`, `fix(store/sqlite): ...`, `docs: ...`). Keep the subject under 72 characters.
