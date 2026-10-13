# ee: the features that need a license key

| Directory | Feature (`license.Feature`) | Needs the key where |
|---|---|---|
| [`oidc/`](oidc/) | `oidc`: OpenID Connect sign-in and bearer ID tokens | `internal/auth/stack` (`Options.OIDCAllowed`), re-checked on every request |
| [`rbac/`](rbac/) | `rbac`: role bindings, scoped roles, Kubernetes SubjectAccessReview delegation, the ClusterRole generator | `internal/rbac.Engine` (`EngineOptions.Licensed`) and `internal/auth/stack`, re-checked on every request |
| [`auditstore/`](auditstore/) | `audit`: the persisted audit trail | `audit.Gated` in `cmd/kudurru/serve.go` and `internal/api` (`/audit`) |
| [`longterm/`](longterm/) | `longterm`: statistics windows older than 24 hours | `internal/api` (`window()`), on every request |
| [`edge/`](edge/) | `edge`: the dnsdist and keepalived supervisors, the VIP providers | `kudurru edge run` and `kudurru vip run` (`cmd/kudurru/license.go`, `Manager.RequireStart`), once at start; refuses only a start with no key that ever covered the edge |
| [`integration/`](integration/) | `integrations`: the integration framework and the external-dns webhook | `cmd/kudurru/serve.go`, at start |

Two more features have no code of their own to move, so they have no
directory here: `requests` (the unblock request workflow; `internal/blockpage`
and `internal/api/unblock.go` check it) and the policy limit, one policy (the
seeded default) without a key (checked in `internal/api/write.go`). Both live
in `internal/license` as rules.

What stays outside `ee/` is what a node runs without a key: the permissions
table, the engine and the built-in administrator (`internal/rbac`), the audit
event types and the log recorder (`internal/audit`), the rollups and the short
windows of the statistics, and the block page.

The [enterprise license notice](LICENSE.md) governs this directory. Eligible
community users may use these features with a valid free entitlement under
the [Community License](../LICENSE). Business and government production use
requires an accepted [commercial agreement](../COMMERCIAL_LICENSE_AGREEMENT.md)
and an Order Form authorizing the Enterprise plan or named modules.
A Commercial Core purchase alone does not unlock `ee/`.

The code builds into the same binary as the core; keys are checked at run
time. How keys work, and the design that preserves DNS answering when a key
lapses, is in [docs/architecture/licensing.md](../docs/architecture/licensing.md).

Rules for code in this directory:

- It may import `internal/...`. `internal/...` may import it only where a
  feature is wired or gated (today `internal/auth/stack`, `internal/api`,
  `internal/health` and `cmd/kudurru`); query-path code (`internal/dnsserver`,
  `internal/pipeline`, `internal/stages`) never does.
- Code that a node without a key runs does not belong here. A feature is
  split along that line: the part every node needs stays in `internal/`, the
  part the key unlocks goes here (`internal/rbac` and `ee/rbac`).
- Do not move something here from the query path. DNS answering, zones,
  blocking, DNS NOTIFY and AXFR stay outside `ee/`.
- A new gate must fail soft: switch the feature off and say why in the log or
  in `GET /api/v1/info/messages`, never stop DNS.
- Add the feature to `license.Features()` and to the table in
  [LICENSING.md](../LICENSING.md).
