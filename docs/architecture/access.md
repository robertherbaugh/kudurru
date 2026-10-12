# Access: sign-in, tokens and RBAC

Everything except DNS goes through the HTTP API on `:8080`: the dashboard is
a single-page app served from `/` that talks only to `/api/v1`, and the CLI
uses the same API with a bearer token. This page shows how a request
becomes a **principal** and how the principal is **authorized**.

Code: `internal/api` (routes, middleware, authorization),
`internal/auth/*` (authenticators, sessions, OIDC, tokens, users, the admin
socket, Kubernetes tokens), `internal/rbac` (`permissions.yaml`, the
engine, SubjectAccessReview), `internal/cluster/internalapi` (node-to-node).

## Principals

| Kind | Comes from | Typical caller |
|---|---|---|
| `User` | local password login, or OIDC sign-in | a person in the dashboard |
| `ServiceAccount` | a `kdr_` API token owned by a kudurru ServiceAccount | automation, the bootstrap token |
| `KubernetesServiceAccount` | a projected ServiceAccount token (audience `kudurru`) | a Job or controller in the cluster |
| `LocalRoot` | a peer on the unix admin socket (uid 0, the server's uid, or a configured gid) | `kudurru admin` inside the container |
| `Node` | the internal node token | another kudurru node, an edge supervisor |

## The request path

```mermaid
flowchart LR
    R(["HTTP request"]) --> M1["recover, request ID,<br/>real IP (trusted proxies),<br/>security headers, access log"]
    M1 --> M2["cross-origin protection<br/>(Sec-Fetch-Site / Origin)"]
    M2 --> AUTHN["authenticate<br/>(/api/v1 only)"]
    AUTHN --> ROUTE["route: each operation declares<br/>resource + verb (+ zone scope)"]
    ROUTE --> AUTHZ["authorize"]
    AUTHZ --> H["handler"]
    H --> AUD["audit: mutations, denials,<br/>runtime actions"]
```

### Authentication: first claim wins

```mermaid
flowchart TD
    R(["request to /api/v1"]) --> B{"Authorization:<br/>Bearer kdr_…?"}
    B -- "yes" --> TOK["API token: look up the id, compare<br/>sha256 in constant time, check expiry"]
    B -- "no" --> J{"Bearer JWT?"}
    J -- "yes" --> ISS{"route by iss"}
    ISS -- "a configured OIDC issuer" --> OIDC["verify ID token (JWKS),<br/>aud, azp, claim mapping"]
    ISS -- "the cluster's issuer" --> K8S["ServiceAccount token:<br/>JWKS offline, else TokenReview<br/>(cached 2 min)"]
    J -- "no" --> CK{"session cookie?"}
    CK -- "yes" --> SES["open the sealed cookie: expiry,<br/>idle timeout, sessionsValidAfter;<br/>unsafe method needs X-Kudurru-CSRF"]
    CK -- "no" --> UX{"unix socket peer?"}
    UX -- "yes" --> ROOT["LocalRoot"]
    UX -- "no" --> NT{"internal node token?"}
    NT -- "yes" --> NODE["Node (viewer + auditor)"]
    NT -- "no" --> ANON["anonymous"]
    TOK --> P(["principal"])
    OIDC --> P
    K8S --> P
    SES --> P
    ROOT --> P
    NODE --> P
```

A credential that is present but wrong is rejected at once (401); it never
falls through to the next authenticator, and an unknown bearer value is
never anonymous.

## Signing in

### First start

When there is no `LocalUser`, the server creates `admin` (built-in admin)
with the password from `KUDURRU_ADMIN_PASSWORD[_FILE]` or
`auth.bootstrap.adminPasswordFile`; otherwise it generates one, prints it
once, and requires a change at the first sign-in (every permission route
answers `403 password_change_required` until then).
`KUDURRU_BOOTSTRAP_TOKEN[_FILE]` creates an admin API token for automation.

Local passwords are Argon2id (19 MiB, t=2, p=1). Failed sign-ins lock an
address for 5 minutes after 5 failures, and an account for 15 minutes after
10.

### OIDC in the browser

kudurru is a confidential client (backend for frontend): the browser never
sees a token, only a sealed cookie.

```mermaid
sequenceDiagram
    autonumber
    actor B as browser
    participant K as kudurru API
    participant I as identity provider
    B->>K: GET /api/v1/auth/oidc/{idp}/start?next=/zones
    K->>K: discovery (lazy), state, nonce, PKCE verifier
    K-->>B: 302 to IdP + sealed pre-auth cookie (10 min, single use)
    B->>I: authorize (code, S256 challenge, nonce)
    I-->>B: 302 /callback?code&state
    B->>K: GET /api/v1/auth/oidc/{idp}/callback
    K->>K: open pre-auth cookie, check state
    K->>I: exchange code + verifier + client secret
    I-->>K: ID token
    K->>K: verify signature, aud, azp, nonce, at_hash
    K->>K: claims → username (prefix) and groups (prefix)
    K->>K: bindings for the user and groups → grants<br/>(none: defaultRole, or 403 no_bindings)
    K-->>B: Set-Cookie session (sealed) + 302 next
    B->>K: GET /api/v1/auth/me → principal, permissions, CSRF token
```

OIDC users are not stored. Their grants are resolved at sign-in and frozen
in the session; deleting or narrowing a binding revokes frozen grants on the
next request. There is no refresh token: the session lasts 12 hours, or
30 minutes without activity.

### The sealed session cookie

```mermaid
flowchart LR
    KEY["KUDURRU_ENCRYPTION_KEY<br/>(+ _PREVIOUS while rotating)"] --> HK1["HKDF-SHA256<br/>info kudurru/session/v1"]
    KEY --> HK2["HKDF-SHA256<br/>info kudurru/oidc-preauth/v1"]
    KEY --> HK3["HKDF-SHA256<br/>info kudurru/internal/v1"]
    KEY --> HK4["HKDF-SHA256<br/>info kudurru/internal-ca/v1"]
    HK1 --> C1["session cookie<br/>AES-256-GCM: sid, principal,<br/>grants, exp, idle_exp, csrf"]
    HK2 --> C2["OIDC pre-auth cookie"]
    HK3 --> C3["node token (:8082)"]
    HK4 --> C4["Ed25519 CA → node<br/>certificates (90 days)"]
```

Because every node derives the same keys from the shared encryption key, any
replica can open any session, and nodes trust each other on `:8082`, without
a shared session store or an extra Secret. The cookie is
`__Host-kudurru_session` when it is Secure (the default, since the API
serves a self-signed certificate unless told otherwise), HttpOnly and
SameSite=Lax, and is re-sealed at most once a minute to slide the idle
timeout.

### The CLI

`kudurru login --token kdr_…` stores a token. `kudurru login --username`
signs in with a password, creates a personal token
(`kudurru-cli@<host>`), signs out, and stores the token. Tokens go to the
OS keyring, with a 0600 file as the fallback. Browser and device-code
sign-in for the CLI are planned for v0.2.

## Authorization

```mermaid
flowchart TD
    R(["route permission"]) --> PUB{"public?"}
    PUB -- "yes" --> H["handler"]
    PUB -- "no" --> WHO{"principal?"}
    WHO -- "no" --> E401["401 unauthenticated"]
    WHO -- "yes" --> AU{"authenticated-only route?"}
    AU -- "yes" --> H
    AU -- "no" --> PW{"must change password?"}
    PW -- "yes" --> E403P["403 password_change_required"]
    PW -- "no" --> ROOT{"LocalRoot?"}
    ROOT -- "yes" --> H
    ROOT -- "no" --> SC{"token scopes cover<br/>resource.verb at scope?"}
    SC -- "no" --> DENY
    SC -- "yes" --> BA{"built-in admin, or an<br/>implicit grant (Node)?"}
    BA -- "yes" --> H
    BA -- "no" --> KSA{"Kubernetes<br/>ServiceAccount?"}
    KSA -- "yes" --> SAR{"SubjectAccessReview on<br/>kudurru.io/<resource> allows?"}
    SAR -- "yes" --> H
    SAR -- "no" --> BIND
    KSA -- "no" --> BIND{"a RoleBinding (or a frozen grant<br/>still backed by one) grants it?"}
    BIND -- "yes" --> H
    BIND -- "no" --> SCOP{"scopable route<br/>(lists, apply)?"}
    SCOP -- "yes" --> HF["handler filters<br/>per object"]
    SCOP -- "no" --> DENY["403 forbidden<br/>hint: missing resource, verb, scope<br/>(audited)"]
```

`internal/rbac/permissions.yaml` is the single source for resources, verbs
and roles; it generates the aggregated ClusterRoles in the chart, and a lint
fails the build when a route declares no permission.

| Role | Can |
|---|---|
| `viewer` | read zones, records, lists, groups, policies, stats, cache, settings (redacted) |
| `auditor` | viewer + the query log (with client addresses), its export, the audit log |
| `operator` | write records, lists, groups, policies, clients; flush the cache; toggle blocking; decide unblock requests |
| `zone-admin` | everything on one zone (the only scoped role: `zone-admin@zone:<name>`) |
| `admin` | `*.*` |

A binding gives a role at a scope (`cluster` or `zone:<name>`) to users,
groups (the prefixed IdP group names) or service accounts. Kubernetes
identities can instead be authorized by Kubernetes RBAC through
SubjectAccessReview on the `kudurru.io` resources, including virtual ones
such as `querylogs` and `caches`; a request is allowed when either path
allows it, and the audit record names the path.

## Node to node (`:8082`)

| Route | Used by |
|---|---|
| `GET /internal/status` | edge supervisors, diagnostics |
| `GET /internal/lists/{digest}` | replicas pulling list bodies from the leader |
| `GET /internal/zones` | edge supervisors (zone serials) |
| `GET /internal/catalogue`, `/internal/catalogue/adguard` | replicas pulling the services catalogue |

Always TLS with the derived CA, always the bearer node token, rate-limited
per client. The same token is accepted on `:8080` as a `Node` principal with
`viewer` and `auditor`.
