# Milestones M3 and M4: API, auth, dashboard, CLI, edge, deploy

These reports record the M3/M4 implementation and measurements before
v0.1.0. For the release's behavior and limitations, read the
[architecture tour](../architecture/README.md) and [changelog](../../CHANGELOG.md).

Six work packages, wired in `cmd/kudurru/serve.go` (commit "feat(serve): wire milestones M3 and M4"). Decisions D98 to D105 in [architecture.md](../architecture.md) record what changed; three integration fixes landed on top (authorization used only name and groups; tokens of local users did not reload their principal; the CLI sent `name` where the server takes `description`).

| Package | Report | Headline |
|---|---|---|
| REST API + external-dns webhook | [api.md](api.md) | 64 paths, 84 operations; envelope, keyset pagination, If-Match, managed_by conflicts, SSE with resume; route-permission lint; generated Go client; webhook conformance against external-dns v0.23 |
| Authentication and authorization | [auth.md](auth.md) | Users, kdr_ tokens, sealed sessions, OIDC (dex e2e), Kubernetes SA tokens with SAR on envtest, RBAC engine, admin socket; 2-6 µs per authenticated request |
| Dashboard | [ui.md](ui.md) | React 19 + Astryx, 183 vitest tests, 235 KiB gzip initial load, embedded with a committed placeholder so Go builds without Bun |
| CLI | [cli.md](cli.md) | 30 top-level commands with golden tests; contexts, keyring, apply/get/describe, dig-like query, logs --follow, admin socket client |
| dnsdist and keepalived supervisors | [edge.md](edge.md) | YAML/Lua renderers with 22 golden files, supervisors tested against fake binaries, `vip drain`, FIFO state file; `make edge-check` for a Docker runner |
| Deployment assets | [deploy.md](deploy.md) | Helm chart validated over 33 permutations with kubeconform, kustomize overlays rendered from the chart, compose profiles, Grafana dashboard generated from registered metrics, goreleaser + release workflow |

Verified live from one config: local login, cookie and token sessions with the admin's roles, `kudurru get zones` / `get blocklists -A` / `query` / `blocking disable 1m` / `apply -f -` (the new record resolved immediately) / `stats summary` / `logs` / `audit`, the real dashboard bundle served with its assets, `/api/v1/openapi.json`.

The original milestone sandbox could not verify Docker edge checks, Helm
on a cluster, kind + MetalLB or two-VM VRRP. The subsequent v0.1 fix round
fixed the keepalived image build and made `make edge-check` a CI gate;
kind + MetalLB runs in the `kind` workflow. The
[final feature review](../m5/features-v0.1.md) records later validation.
Verify client identity and VIP failover in each deployment using the
[release checklist](../release-checklist.md#3-checks-before-ga-ci-and-the-owners-hardware).
