# Deploying kudurru

| Shape | Guide | Assets |
|---|---|---|
| One container (Docker, Podman) | [docker.md](docker.md) | `deploy/compose/single`, `deploy/compose/single-edge` |
| Three hosts with a VIP (dnsdist + keepalived, `ha-static`) | [compose-ha.md](compose-ha.md) | `deploy/compose/ha/node-{a,b,c}` |
| Kubernetes with Helm | [helm.md](helm.md) | `charts/kudurru`, `charts/kudurru-crds` |
| Kubernetes with kustomize | [kustomize.md](kustomize.md) | `config/` |
| Linux packages (deb, rpm, apk) with systemd | [docker.md#linux-packages-and-systemd](docker.md#linux-packages-and-systemd) | `deploy/systemd` |
| Monitoring | [helm.md#monitoring](helm.md#monitoring) | `deploy/grafana/kudurru-overview.json`, `deploy/prometheus/rules.yaml` |

Runbooks: [cold start without the apiserver, key rotation, upgrades](runbooks.md).
Blocked services and safe search, and the runtime catalogue URL: [blocked-services.md](blocked-services.md).
Allowlist, Denylist and switching single entries of any list off: [allowlist.md](allowlist.md).
The block page (blocked names open a page explaining the block, with a Request access form administrators answer on the Requests page): [blocking-page.md](blocking-page.md).

## Release artifacts

Releases are cut by merging `main` into `release-candidate` (a
`v<version>-rc.<N>` pre-release) and `release-candidate` into `release` (the
`v<version>` release, which also moves `latest`); the version comes from the
`VERSION` file ([release-checklist.md](../release-checklist.md#release-pipeline)).
Both run `.github/workflows/publish.yml`, which runs goreleaser
(`.goreleaser.yaml`) and publishes:

- `ghcr.io/robertherbaugh/kudurru:<version>`: distroless static, nonroot
  (uid 65532), `linux/amd64` and `linux/arm64`, built by ko, with an SPDX SBOM
  and a cosign keyless signature; `<version>-debug` adds busybox (a shell and
  `nslookup`); a release also moves `latest` (and `latest-debug`), a
  release candidate does not;
- binaries for linux and darwin on amd64 and arm64, deb/rpm/apk packages with
  a hardened systemd unit, a compose bundle, `crds.yaml` and `install.yaml`
  (`kustomize build config/default`), all covered by `checksums.txt`, which is
  cosign-signed, plus GitHub build provenance;
- the Helm charts at `oci://ghcr.io/robertherbaugh/charts/kudurru` and
  `.../kudurru-crds`, cosign-signed.

Every pull request against `main` also pushes
`ghcr.io/robertherbaugh/kudurru:dev-<PR number>` (unsigned, for testing the
branch; later pushes overwrite it).

Verify an image:

```sh
cosign verify ghcr.io/robertherbaugh/kudurru:0.1.0 \
  --certificate-identity-regexp '^https://github\.com/robertherbaugh/kudurru/\.github/workflows/publish\.yml@refs/heads/release(-candidate)?$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Ports

| Port | Protocol | Purpose |
|---|---|---|
| 53 | udp, tcp | DNS (5353 PROXYv2-only behind the dnsdist edge) |
| 853 | tcp / udp | DNS over TLS / DNS over QUIC |
| 443 | tcp / udp | DNS over HTTPS / HTTP/3 |
| 8080 | tcp | API and UI |
| 8081 | tcp | ops: `/healthz/startup`, `/healthz/ready`, `/healthz/live` (no auth) |
| 8082 | tcp | internal node-to-node listener (always TLS) |
| 8084 | tcp | edge supervisor readiness |
| 80, 443 | tcp | block page (optional, `listeners.blockPage`; 8090/8493 in the chart and compose containers) |
| 8888 | tcp | external-dns webhook provider (optional) |
| 9153 | tcp | Prometheus `/metrics` |

## Secrets

The binary reads secrets from files named by environment variables, never
from the config file:

| Variable | Content |
|---|---|
| `KUDURRU_ENCRYPTION_KEY_FILE` | 32 random bytes, base64 or 64 hex digits (`openssl rand -base64 32`). Seals sessions and stored secrets and derives the internal listener's token and CA (D47, D92). Required in Kubernetes; every node of a cluster shares it. |
| `KUDURRU_ENCRYPTION_KEY_PREVIOUS_FILE` | The previous key during a rotation ([runbooks.md](runbooks.md#rotating-the-encryption-key)). |
| `KUDURRU_ADMIN_PASSWORD_FILE` | Password of the first-start `admin` user; read only while no user exists. |
| `KUDURRU_BOOTSTRAP_TOKEN_FILE` | Optional `kdr_<id>_<secret>` token created at first start for automation. |

Every settings key can also be set as `KUDURRU_<SECTION>__<KEY>` (for example
`KUDURRU_LOG__LEVEL=debug`) or `KUDURRU_<SECTION>__<KEY>_FILE`; list and map
keys (listeners, upstream groups) only from the file. An environment variable
that names no key stops the server at start, so typos do not go unnoticed.
`kudurru config schema` lists every key.
