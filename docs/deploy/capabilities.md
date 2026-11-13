# What works where, and what a refusal means

kudurru runs in a few deployment shapes, and some API commands work only in
some of them. A command that a shape does not offer is refused with a JSON
error that has a stable `key`, a `message` and usually a `hint`; it is never
attempted halfway. This page lists, for each shape, which commands work, and
then explains every refusal key that names a deployment limit.

The shapes:

- **Standalone**: a SQLite node that is not in a cluster
  ([docker.md](docker.md)).
- **Static**: a SQLite node with `cluster.role: static`, one of the Compose
  HA twins whose objects come from files ([compose-ha.md](compose-ha.md)).
- **Primary** and **follower**: the members of an enrolled SQLite cluster
  ([sqlite-cluster.md](sqlite-cluster.md)).
- **Fenced or conflicted**: a former primary that has seen a newer one, or a
  member that recorded two promotions
  ([sqlite-join.md](sqlite-join.md#reconnecting-an-old-primary)).
- **Kubernetes**: the Kubernetes store, installed with Helm or kustomize
  ([helm.md](helm.md)).

## Capabilities by shape

"Yes" means the command works there, subject to the usual permission
checks. A key in the table is the answer you get instead; the
[glossary](#refusal-keys) says what it means and what to do. A static node and
a follower answer reads (`GET`) the same way as the standalone node unless a
row says otherwise. A fenced or conflicted member answers every API request
with its own key, so that column is the same in every row:
`cluster_fenced` or `cluster_conflict`.

| Capability | Standalone and primary | Static | Follower | Kubernetes |
|---|---|---|---|---|
| Zones, record sets, block lists, client groups and policies (`kudurru apply`, the dashboard editors, `kudurru delete`) | Yes | `read_only`: edit the object files on every node | Forwarded to the primary | Yes |
| Turning blocking off and on | Yes | Yes, for this node only (`scope: node`) | Forwarded | Yes, for every replica |
| RFC 2136 updates ([dynamic-updates.md](dynamic-updates.md)) | Yes | Yes, for this node only: static twins do not replicate updates | Not forwarded: send them to the primary | Yes |
| Stored TSIG keys (`/tsigkeys`, [tsig-keys.md](tsig-keys.md)) | Yes | Reads only: changes answer `read_only` | Reads; changes answer `primary_required` | Yes |
| Cluster settings (`kudurru settings set` of a cluster key) | Yes | `read_only` | Forwarded | Yes |
| Node settings (`PATCH /node/config`) | Yes, for keys that the node's file or environment does not set (`host_locked`, `env_locked` otherwise) | As standalone | `primary_required`: set them in the node's file or environment | `read_only` (HTTP 501): set them in the chart values or the node's environment |
| Settings ownership and YAML export (`kudurru settings ownership`, `settings yaml`) | `unavailable`: the database holds the settings; use `kudurru settings get` | `unavailable`, as standalone | `unavailable`, as standalone | Yes |
| The five sign-in controls (`auth.localLogin`, `auth.session.idleTimeout`, `auth.session.lifetime`, `auth.session.secure`, `auth.tokens.maxLifetime`) | Take effect without a restart | `read_only` | Forwarded | Need a restart of the replicas (`restartRequired` names them) |
| Password sign-in in the browser and sign-out | Yes | Yes | Forwarded; needs a reachable primary | Yes |
| `kudurru login --username` (the CLI saves an API token) | Yes | Fails: the node cannot create the token (see below) | `primary_required` | Yes |
| Your own API tokens | Yes | `read_only` | `primary_required` | Yes |
| OIDC sign-in in the browser | Yes | Yes | On the primary only: a follower lists no provider | Yes |
| OIDC sign-in from the CLI (`kudurru login` with a provider) | Yes | Yes | `primary_required` | `cli_login_unavailable` |
| Provider sign-out (logout at the identity provider) | Yes | Yes | On the primary only | `provider_logout_unavailable`: the local session ends, end the provider session at the provider |
| Authenticator codes (TOTP) | Yes | `read_only` for setup and removal | `primary_required` | `factor_management_unavailable`; a sign-in with a code answers `factor_unavailable` |
| Session inventory (`GET /auth/sessions`, revoking a session) | Yes | Yes | Reading yes; revoking your own session is forwarded | `sessions_unavailable` |
| External accounts (`/iam/external-users`) | Yes | Reads only | Reads; changes answer `primary_required` | `external_users_unavailable` |
| Local users (`POST /iam/users`, password resets) | Yes | `read_only` | `primary_required` | `user_management_unavailable` |
| Role bindings through the API (`/iam/rolebindings`, [rolebindings.md](rolebindings.md)) | Yes | `read_only` | `primary_required` | `unavailable`: change the `RoleBinding` objects with `kubectl` |
| Cluster enrollment, invitations and promotion (`kudurru cluster ...`) | Yes | `unavailable` | Promotion runs on the follower; activation and invitations answer `409 conflict` | `unavailable` |
| Background integrations ([integrations.md](integrations.md)) | Yes, with a license key | Reads; commands answer `read_only` | Reads; commands answer `primary_required` | Yes, with a license key, for Cloudflare and UniFi inventory in the release namespace; check, preview, sync and actions run on the replica that holds the Integration's Lease, and another replica answers `503` with `hint.holder` |

Static nodes accept no credential changes through the API. To use the CLI
against one, start it with a token you generated yourself
(`kudurru token generate`, passed as `KUDURRU_BOOTSTRAP_TOKEN`) and save it
with `kudurru login --token-stdin`. `kudurru login --username` against a static
node ends with `mint a CLI token: ... request failed; outcome may be unknown`:
no token was created, but the password sign-in before it left a session for
the account. That session lasts until it expires or you revoke it
(`DELETE /api/v1/auth/sessions/{id}` works on a static node).

[What a follower forwards, runs itself and refuses](sqlite-join.md#what-a-follower-forwards-runs-itself-and-refuses)
lists every route of a follower; `GET /api/v1/cluster` on a follower names the
forwarded and local operations, and the OpenAPI document carries the same
class as `x-kudurru-follower`.

## Refusal keys

Each key below comes with HTTP 503 unless the table says otherwise. None of
them is fixed by sending the same request again, except `generation_pending`.

| Key | Status | Meaning | What to do |
|---|---|---|---|
| <a id="read_only"></a>`read_only` | 503 | A static node: objects, settings, credentials and provider commands are read-only through the API. | Edit the distributed files on every node, and provision accounts and tokens before you enable static mode. |
| `read_only` | 501 | `PATCH /node/config` on a server without a node configuration writer (the Kubernetes store). | Set the key in the node's file, environment or chart values. |
| <a id="host_locked"></a>`host_locked` | 409 | The node's own file sets this node key, so the API may not change it. | Change it in that file, or remove it there to hand it to the API. |
| <a id="env_locked"></a>`env_locked` | 409 | A `KUDURRU_*` variable of the node sets this node key. | Change or unset the variable on the node. |
| <a id="primary_required"></a>`primary_required` | 503 | An enrolled follower does not forward this change and does not run it. Nothing was sent or changed. The message names the primary. | Send it to the primary. It is the same while the primary is unreachable. |
| <a id="completion_unknown"></a>`completion_unknown` | 503 | The follower sent the change, or a cluster command was sent, and the answer did not arrive. The change may or may not have been made. | Read the object or the enrollment, on the primary when you can, before you decide anything else. Do not replay it. |
| <a id="committed_output_unavailable"></a>`committed_output_unavailable` | 503 | `kudurru cluster join-token`: the invitation was committed, but its secret could not be returned. | Do not replay the command. Read the enrollment, and issue a new invitation for the member if you need one. |
| <a id="cluster_fenced"></a>`cluster_fenced` | 503 | This former primary has seen a newer primary. Its API is closed. | It follows the newer primary at its next successful pull; see [reconnecting an old primary](sqlite-join.md#reconnecting-an-old-primary). |
| <a id="cluster_conflict"></a>`cluster_conflict` | 503 | This member recorded two promotions. Its API is closed. | Resolve the conflict offline ([recover from conflicting promotions](sqlite-join.md#recover-from-conflicting-promotions)). |
| <a id="generation_pending"></a>`generation_pending` | 503, `Retry-After: 1` | The node is still preparing its next configuration, for example right after a change. | Wait a moment and send the request again. |
| <a id="unavailable"></a>`unavailable` | 503 | The command cannot run on this node now or in this shape; the message says which. "current writable serving generation unavailable" with `Retry-After: 1` means that no part of the command ran: the node kept publishing other commits for five seconds while the command waited to be admitted, or the caller's credential, roles or the cluster settings changed meanwhile; the same request may be sent again. Without `Retry-After` the command was refused inside or after its transaction; review the current state first. Other examples: an item of `POST /apply` that another change cut ("another change was committed on this node while the request ran"), a follower whose data changed while the request ran, settings ownership on a SQLite node, cluster enrollment on a static node or on Kubernetes, API role bindings on Kubernetes, and other authentication routes on Kubernetes ("this operation is unavailable on this server"). | Read the hint. When the message says the item was not applied, review the current state before you send a new request. |
| <a id="factor_management_unavailable"></a>`factor_management_unavailable` | 503 | Kubernetes: authenticator setup, confirmation, removal and recovery need the SQLite runtime. | Use password sign-in, OIDC with the provider's own second factor, or API tokens. |
| <a id="factor_unavailable"></a>`factor_unavailable` | 503 | Kubernetes: a sign-in or password change with an authenticator code needs the SQLite runtime. | Sign in without a code. |
| <a id="user_management_unavailable"></a>`user_management_unavailable` | 503 | Kubernetes: creating local users and resetting their passwords through the API needs the SQLite runtime. | Reset a password on the replica's admin socket (`kubectl exec ... -- kudurru admin reset-password NAME`), and give other people access through OIDC and role bindings. |
| <a id="sessions_unavailable"></a>`sessions_unavailable` | 503 | Kubernetes: the list of sign-ins and revoking one need the SQLite runtime. | Sign out in the browser; sessions end at their lifetime. |
| <a id="external_users_unavailable"></a>`external_users_unavailable` | 503 | Kubernetes: the administration of external (OIDC) accounts needs the SQLite runtime. | Manage the accounts at the identity provider and with role bindings. |
| <a id="cli_login_unavailable"></a>`cli_login_unavailable` | 503 | Kubernetes: a command-line sign-in with an identity provider needs the SQLite runtime. | `kudurru login --token` or `kudurru login --username`, as the hint says. |
| <a id="provider_logout_unavailable"></a>`provider_logout_unavailable` | 503 | Kubernetes: the continuation of a sign-out at the identity provider. The local session has ended. | End the provider session at the identity provider. |
| <a id="integration_uncertain"></a>`integration_uncertain` | 503 | An integration command (check, preview, sync, an action) was not acknowledged; it may have had effects, which were not repeated. | Read the integration's status and its actions before a new command. |
| <a id="not_implemented"></a>`not_implemented` | 501 | The integration provider does not offer this command. | Use a command that the provider's actions list names. |

The CLI prints the message and the hint of these answers, and adds its own
hint for `primary_required`, `completion_unknown`, `cluster_fenced` and
`cluster_conflict`. [API contracts](api-contracts.md) describes the answers
of `POST /apply` item by item.
