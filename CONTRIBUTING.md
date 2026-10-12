# Contributing to kudurru

Thank you for helping build kudurru. This file covers the rules that are easy to get wrong; how the code fits together is in the [architecture tour](docs/architecture/README.md), and the design and its decision log are in the [design record](docs/architecture.md).

## Licensing rule (read this first)

kudurru is source-available under the [PolyForm Noncommercial License 1.0.0](LICENSE), and the code under `ee/` needs a license key ([LICENSING.md](LICENSING.md)). Because the project is not open source and its owner also offers commercial licences, no GPL, EUPL or other copyleft code can ever be part of it. Several of the projects it learns from are **GPL or EUPL**: Gravity (GPL-3.0), Technitium (GPL-3.0), AdGuard Home and urlfilter (GPL-3.0), Pi-hole (EUPL-1.2), dnsdist and keepalived (GPL-2.0). Code, markup, and data from those projects must never be copied, translated, or "ported" into this repository. Reading their docs and describing their behaviour is fine; the research notes under `docs/research/` do exactly that. Reimplement from the specification or from a permissively licensed reference (blocky, dnsproxy, CoreDNS, external-dns; see [NOTICE](NOTICE)).

dnsdist and keepalived are run as separate programs in their own images under `images/`. They are never linked, vendored, or patched.

CI runs `go-licenses` with a deny list for GPL and EUPL modules. Embedded data (the starter blocklist) must come from permissively licensed sources with a provenance file.

## Contributor agreement

The owner offers kudurru under commercial licences as well as the noncommercial one, so every contribution has to be licensable on both terms. Every commit must carry a `Signed-off-by: Name <email>` line (`git commit -s`). By adding it you certify the [Developer Certificate of Origin 1.1](https://developercertificate.org/) and you also grant Robert Herbaugh, the project owner, a perpetual, worldwide, non-exclusive, royalty-free, irrevocable licence to use, reproduce, modify, sublicense and distribute your contribution under any terms, including commercial and proprietary ones. You keep your copyright. Pull requests without the sign-off are not merged.

> These terms are a draft that has not had legal review. Do not accept outside contributions before they have.

## Building

```sh
make build        # bin/kudurru
make test         # unit tests with -race
make lint         # golangci-lint, go vet, generated-file drift
make generate     # controller-gen: deepcopy, CRDs, RBAC
```

Go 1.27 is required. `CGO_ENABLED=0` is a release requirement; do not add cgo dependencies.

## Branches and releases

Open pull requests against `main`; each push to one builds `ghcr.io/robertherbaugh/kudurru:dev-<PR number>` for trying the branch. Releases are cut by merging `main` into `release-candidate` (tags `v<VERSION>-rc.<N>`, a GitHub pre-release) and `release-candidate` into `release` (tags `v<VERSION>`, moves `latest`), where `VERSION` is the file in the repository root. Never push release tags by hand. The steps and the checks around them are in [docs/release-checklist.md](docs/release-checklist.md#release-pipeline).

## Layout and seams

- `api/v1alpha1/` holds the Go types for every kind. The same structs are the CRDs, what the SQLite driver persists, and what the REST API, CLI and UI exchange. Change the type, run `make generate`, and all three follow.
- `internal/store` is the persistence seam. Serving code never reads the store on the query path; it reads snapshots materialised by `zonecache`.
- `internal/pipeline` defines the fixed stage chain. Stage order is code, not configuration.
- `ee/integration` is the only way to add outbound sinks (Cloudflare) or inbound sources (UniFi). Integrations never run on the query path.
- `internal/rbac/permissions.yaml` is the single source for roles, Kubernetes ClusterRoles and the docs table. Every API route declares its permission; a lint fails the build otherwise.
- `internal/config/schema.go` describes every settings key. Keys flagged `hostOnly` can never be set through the API.

Deferred features have seams documented in [architecture.md §13.1](docs/architecture.md#131-v01--stone-a-credible-daily-driver-16-weeks-one-release-cycle). Land a deferred feature by filling its seam, not by adding a parallel path.

## Decisions

Architecture changes go through the decision log in [architecture.md §16](docs/architecture.md#16-decision-log): add a row with the topic, the decision, why, and the alternatives rejected. Open questions live in [docs/open-questions.md](docs/open-questions.md).

## Commit messages

Conventional commits (`feat(blocking): ...`, `fix(store/sqlite): ...`, `docs: ...`). Keep the subject under 72 characters.
