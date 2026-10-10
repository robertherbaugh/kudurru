# Research notes

Point-in-time studies (2026-10-10) of the projects kudurru draws on. The [architecture](../architecture.md) cites them with short keys:

| Key in citations | File | Subject |
|---|---|---|
| gravity | [gravity.md](gravity.md) | BeryJu/gravity: roles, embedded etcd, replication, API/UI, pitfalls |
| technitium | [technitium.md](technitium.md) | Technitium DNS Server feature inventory, apps, permissions |
| pihole | [pihole.md](pihole.md) | Pi-hole v6 dashboard, FTL API, query log storage, UX principles |
| blocking | [blocking-engines.md](blocking-engines.md) | blocky and AdGuard Home: list formats, matchers, policies, licences |
| dnsdist | [dnsdist-keepalived.md](dnsdist-keepalived.md) | dnsdist config/API/console, keepalived VRRP, container topologies |
| k8s | [k8s-native-dns.md](k8s-native-dns.md) | CRDs, operators, external-dns, LoadBalancer realities, RBAC mapping |
| auth | [auth-oidc-rbac.md](auth-oidc-rbac.md) | OIDC flows, sessions, tokens, RBAC models, security details |
| integrations | [integrations.md](integrations.md) | Extension models, Cloudflare API, UniFi API, plan/ledger design |
| go-dns-stack | [go-dns-stack.md](go-dns-stack.md) | Go libraries, storage options, frontend stack, release tooling |
| observability | [observability-querylog.md](observability-querylog.md) | Query log pipeline, SQLite schema, metrics, dnstap, multi-node stats |

These notes describe other projects for analysis. Nothing in them may be ported into kudurru; see [CONTRIBUTING.md](../../CONTRIBUTING.md) for the licensing rule.
