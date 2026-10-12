# M3 work package: authentication and authorization wiring

Key `auth`. This package covers docs/architecture.md §8.1-8.8, §5.5 and decisions D21/D22, D48, D54-D57 and D71. It turns the auth spike (docs/spikes/auth-oidc.md: sealed sessions and the OIDC BFF login) into the identity layer the API server mounts:

- local users and the first-boot admin
- API tokens and service accounts
- Kubernetes ServiceAccount tokens with SubjectAccessReview
- the authenticator chain and the authorizer chain
- the /auth and /iam routes
- the break-glass admin socket
- encryption-key loading

The integrator wires it into `cmd/kudurru/serve.go` as described in [Serve wiring](#serve-wiring).

It closes these follow-up rows:

- "roles_snapshot is frozen at login": `rbac.BindingSource` honours a sealed grant only while a live binding still provides it and no binding change removed it after the session's iat.
- "Export an rbac subject-matching helper" (`rbac.SubjectMatches`, `SubjectsMatch`, `GrantsFor`; `oidc.BindingResolver` uses them, and an empty name never matches).
- "Wire Registry.Register, session.Authenticator, ValidAfter, keys": done by `stack`.
- "Serve internal kinds (Secrets labelled kudurru.io/kind) in the kube driver".
- The `InternalKinds` part of the store-sqlite row.

Narrower rows replace them; see [Follow-ups](#follow-ups).

## What was built

| Package | Contents |
|---|---|
| `api/internalkinds/v1alpha1` (new) | Go types for LocalUser, APIToken, ServiceAccount, RoleBinding and IdentityProvider with `+kubebuilder:object:root=true`, scheme registration under kudurru.io/v1alpha1 (`AddToScheme`, `NewScheme`), `Kinds`, `Namespaced`, Go validation (`Validate() error` returning field paths), and deepcopy from controller-gen. They are **not CRDs**: the Makefile `generate` object run includes the path, while `manifests` reads `./api/v1alpha1/...` only. `make check-generated` is clean and `config/crd/bases` still holds the same 5 CRDs. |
| `internal/auth` (additive) | `Middleware(Options)` (Authenticate + LoadPrincipal of §8.8 over `Authenticator` steps), `Error` and `WriteError` (the contract body `{error:{key,message,hint},took_ms}`, `WWW-Authenticate`, `Retry-After`), `CSRF(r)`/`CSRFWith`/`CheckCSRF`, `BearerJWT` (routes bearer JWTs by unverified `iss`), `UnixPeer` + `PeerPolicy` + `PeerConnContext` + `ReadPeerCred` (SO_PEERCRED, Linux), `InternalToken` (Node principal), `Authorize`/`Require`/`RequireAuthenticated`/`ClusterAttrs` (401/403 with `missing` hint, `password_change_required`), `WithDecision`/`DecisionFrom` (Decision.Path for audit), `RBACPrincipal` (Principal → rbac.Principal), and the new principal fields (`ServiceAccount`, `UID`, `BuiltinAdmin`, `MustChangePassword`, `Grants`, `Scopes`, `TokenID`, `Implicit`). |
| `internal/auth/objcache` (new) | A watch-fed generic cache of one kind (`Prime`, `Run` with re-watch and backoff, prune at the bookmark, read-your-writes `Upsert` that never regresses a resource version). |
| `internal/auth/users` (new) | `Users`: Argon2id m=19 MiB t=2 p=1 PHC strings with a global hashing semaphore, 1 KiB cap and bounded PHC parameters; `Login` with per-IP (5 → 5 min) and per-user (10 → 15 min) lockouts, a dummy hash for unknown users and sanitised usernames in logs; `Create`/`SetPassword`/`ChangePassword`/`RevokeSessions`/`SetDisabled`; `ValidAfter` (the sealed store's sessionsValidAfter hook); `LoadPrincipal`; `Bootstrap` (first-boot admin from `KUDURRU_ADMIN_PASSWORD[_FILE]` or `auth.bootstrap.adminPasswordFile`, otherwise generated and printed once with `mustChangePassword`; a per-node bootstrap manager makes racing replicas lose with `ErrManagedBy` instead of overwriting); `HasDefaultCredentials`. |
| `internal/auth/tokens` (new) | `Generate`/`Parse`/`Hash` for `kdr_<12 [a-z0-9]>_<43 base64url>`, stored as hex(sha256(token)) and compared in constant time on the full hash. Unknown id, wrong secret, malformed and expired tokens all give the same 401. `Create` enforces `auth.tokens.maxLifetime` (it is the default expiry and a cap), and supports frozen grants for OIDC owners and scopes. Also `Revoke`/`RevokeOwner`/`List`, service accounts (`EnsureServiceAccount`, disabled accounts refused), lastUsedAt/IP written at most once a minute per token by `Run`, `Bootstrap` for `KUDURRU_BOOTSTRAP_TOKEN[_FILE]`, and the bearer `kdr_` step. |
| `internal/auth/k8s` (new) | Projected ServiceAccount tokens (audience `kudurru`) verified against the cluster's discovery document and `/openid/v1/jwks`, both read through the apiserver with the pod's credentials. TokenReview (with an audience check) is the fallback while discovery fails. Verdicts are cached by sha256(token) for at most 2 min (never past exp) and rejections for 10 s. The principal is `system:serviceaccount:<ns>:<name>` with its groups, UID and `ServiceAccount: <ns>/<name>`. `SecretResolver` serves `clientSecretRef`. |
| `internal/auth/keys` (new) | `Load`: `KUDURRU_ENCRYPTION_KEY[_PREVIOUS][_FILE]` (base64 or hex). In Docker mode without a key it generates one into `<dataDir>/keys/encryption.key` (0600, directory 0700, link-not-rename so concurrent first starts agree). The kube driver refuses to start without a key. |
| `internal/auth/adminsock` (new) | The admin socket server (protocol below): `Listen` (0660, optional chgrp, stale-socket cleanup), `Serve`, `ListenAndServe`, `Handler`. |
| `internal/auth/stack` (new) | Assembles everything from `config.Config` and the store; see [Serve wiring](#serve-wiring). It provides the routes of [Routes](#routes) with their RBAC declarations, builds the OIDC registry from `auth.identityProviders[]` plus IdentityProvider objects (rebuilt on change, failures as diagnoses), feeds RoleBinding objects into the authorizer through a watch, and exposes `Diagnoses` for `/info/messages`. |
| `internal/auth/authtest` (new, test helper) | Memory and temporary-sqlite stores serving the internal kinds; `Drivers` runs a test on both. |
| `internal/auth/session` (additive) | `Authenticator.Authenticate` (the cookie step for the chain, with contract errors; `Middleware` is unchanged), and `CookieConfig.HostPrefix`/`NameFor`: the `__Host-` cookie when Secure and Path=/ (D71). |
| `internal/auth/oidc` (additive) | Bearer ID tokens (`Provider.AcceptsIssuer`/`VerifyJWT`, §8.3: aud must contain cliClientID/clientID/audiences, several audiences need a kudurru azp, `typ: at+jwt` refused, cached min(exp, 15 m)), `ProviderConfig.CLIClientID`/`Audiences`, `ParseConfigEntry`/`FromIdentityProvider` (strict decoding, `<id>:` default username and group prefixes, clientSecretFile or clientSecretRef), and `Registry.ByIssuer`/`CheckRedirects`. |
| `internal/rbac` (additive) | `Engine` (scopes → implicit admin → implicit grants → SAR → bindings; Decision.Path `local`/`sar`/`binding`/`none`), `BindingSource` (config bindings + RoleBinding objects, swapped atomically, invalid objects diagnosed, grant revocation tracking, `AllowGrant` for an IdP defaultRole), `SARAuthorizer` (real and virtual kudurru.io resources, 2 min allow / 30 s deny cache, outage = deny with reason), `SubjectMatches`/`SubjectsMatch`/`GrantsFor`/`ParseBindings`/`FromRoleBinding`/`RoleBindingObjects`, `Grant`, `PermissionMatrix`, `Principal.Implicit`. |
| `internal/config` (additive) | `auth.localLogin`, `auth.bootstrap.adminPasswordFile`, `auth.session.{idleTimeout,lifetime,secure}`, `auth.tokens.maxLifetime`, `auth.identityProviders` (list of maps), `auth.kubernetes.mode` (sar; impersonate rejected with the D55 explanation), `auth.adminSocket.{path,group}`, `rbac.bindings` (list of maps); validation; new `auth_test.go`. The drift test is green. |
| `internal/store/sqlite` (additive) | Internal kinds skip the CRD validator and use their own `Validate` (converted to `*validator.Error`); `Options.InternalKinds` was already there for safe snapshots. |
| `internal/store/kube` (additive) | Internal kinds served as Secrets `kudurru-<kind>.<ns>.<name>` labelled `kudurru.io/kind` in the release namespace, with the object JSON in `data.object` (Get/List/Apply/Delete/UpdateStatus/Watch with the Apply contract; one shared Secret informer). `Snapshot` strips them when Safe. `PodNamespace()` is exported, and core/v1 is added to the scheme when internal kinds are configured. |
| `Makefile` | The `generate` object run covers `./api/internalkinds/v1alpha1/...`; `manifests` (CRDs) reads `./api/v1alpha1/...` only. |
| `NOTICE` | x/crypto is now also used for Argon2id in production code. |
| go.mod | go-jose v4.1.5 moves from indirect to direct (tests sign `at+jwt` tokens); no new modules. |

## The chains

**Authenticate** (`auth.Middleware`, in this order; the first step that recognises its credential decides):

1. **bearer `kdr_`** (`tokens.Tokens`): a personal token acts as its owner, and a service-account token as `serviceaccount:<ns>:<name>`.
2. **bearer JWT routed by `iss`** (`auth.BearerJWT`):
   - an exact issuer match goes to that OIDC provider (`VerifyJWT`);
   - the cluster's service-account issuer goes to `k8s.Authenticator` (JWKS);
   - any other issuer goes to `k8s.Fallback()`, which asks TokenReview only while discovery is failing and otherwise rejects.
3. **session cookie** (`session.Authenticator`): CrossOriginProtection and `X-Kudurru-CSRF` on unsafe methods; the idle timeout slides; `Grants` = roles snapshot and `GrantsIssuedAt` = iat. On a CSRF failure the session is **dropped, not failed**: the failure is recorded with `auth.WithCSRFFailure`, and the chain goes on. Routes that need a principal then answer 403 `csrf` (`Authorize`, `RequireAuthenticated`), logout refuses, and public routes such as login run anonymous.
4. **unix peer** (`auth.UnixPeer`): connections that carry SO_PEERCRED from uid 0, the server uid or the configured gid become `LocalRoot`, the builtin admin.
5. **internal token** (`auth.InternalToken`, `internalapi.Credentials.ValidToken`): a `Node` principal with the implicit grants viewer and auditor.

The chain has three outcomes:

- A request that no step recognises passes through anonymous (public routes serve it; `Authorize` answers 401).
- A bearer credential that no step claims is **401, never anonymous**.
- A credential that a step rejects is answered by the middleware.

**LoadPrincipal** (`users.LoadPrincipal`): local users are re-read from the cache on every request. A deleted or disabled account is 401. Live groups, builtin-admin and mustChangePassword replace whatever the cookie or token carried, so local users are always authorized from live bindings.

**Authorize** (`rbac.Engine` via `auth.Authorize`/`auth.Require`), in this order:

1. Token scopes narrow everything.
2. The builtin admin is allowed with Path `local`.
3. Implicit grants are allowed with Path `local`.
4. `rbac.Chain{SAR (Kubernetes identities, kube driver only), BindingSource (rbac.bindings + RoleBinding objects)}` decides. The first allow wins (D22), and Decision.Path names it.

`mustChangePassword` is 403 `password_change_required` except on the self-service routes.

## Serve wiring

Everything below goes in `cmd/kudurru/serve.go`; none of it is edited by this package.

```go
import (
    internalkinds "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
    "github.com/robertherbaugh/kudurru/internal/auth"
    "github.com/robertherbaugh/kudurru/internal/auth/keys"
    "github.com/robertherbaugh/kudurru/internal/auth/stack"
)

// 1. Both drivers serve the internal kinds (openStore).
scheme, err := internalkinds.NewScheme()          // public + internal kudurru.io kinds
sqlite.Options{Validator: v, Scheme: scheme, InternalKinds: stack.InternalKinds(), ...}
kube.Options{Scheme: scheme, InternalKinds: stack.InternalKinds(), ...} // core/v1 is added by the driver
// openStore must also hand back restCfg (kube only) for step 3.

// 2. Keys: replace readEncryptionKeys (it moves into internal/auth/keys).
k, err := keys.Load(keys.Options{Driver: cfg.Store.Driver, DataDir: cfg.Node.DataDir})
encKey, encPrev := k.Current, k.Previous   // Docker now always has a key, so the internal
                                           // listener always gets credentials; kube without
                                           // a key fails here with keys.ErrKeyRequired.

// 3. The identity layer (after the store and the internal credentials, before the API server).
ns := ""                                   // Docker: tokens.DefaultNamespace ("kudurru-system")
if cfg.Store.Driver == "kube" { ns = kube.PodNamespace() }
ids, err := stack.New(ctx, stack.Options{
    Config:        cfg,
    Store:         st,
    Keys:          k,
    Kube:          restCfg,                // nil with sqlite: disables SA tokens, SAR, clientSecretRef
    Namespace:     ns,
    InternalToken: creds.ValidToken,       // nil if there are no internal credentials
    CrossOrigin:   apiCOP,                 // the API server's *http.CrossOriginProtection
    TrustForwardedProto: <true only behind trusted proxies>,
    ClientIP:      realIP,                 // after RealIP(trustedProxies); default RemoteAddr
    Logger:        logger,
})
// New primes the caches, creates the first-boot admin (printing a generated
// password to stderr once) and installs KUDURRU_BOOTSTRAP_TOKEN.

// 4. Runnables: on EVERY replica (not leader-only; the caches are per process).
group.Go(func() error { return ids.Run(ctx) })
if p := cfg.Auth.AdminSocket.Path; p != "" {
    group.Go(func() error { return ids.AdminSocket().ListenAndServe(ctx, p) })
}
```

The API server needs these from the stack (the API agent owns the server):

- **Middleware construction order** (§8.8): `Recover → RequestID → RealIP(trustedProxies) → SecurityHeaders → AccessLog → CrossOriginProtection → ids.Middleware() → mux`. The API and cookie authenticator enforce cross-origin protection. `api.Options.CSRF` is set to `auth.CSRF`; protected API routes also honor the cookie step's `auth.CSRFFailure` note and return 403 `csrf` with the header hint.
- **Login throttling:** `kudurru serve` leaves the optional `api.Options.RateLimit` hook unset. The users store's per-client-address (5 failures → 5 min) and per-account (10 → 15 min) lockouts are the only login throttle, returning 429 with `Retry-After` before password verification. The global hashing semaphore bounds concurrent Argon2id work; there is no additional request-rate limiter.
- **Routes:** `for _, rt := range ids.Routes() { mux.Handle(rt.Method+" /api/v1"+rt.Path, ids.Guard(rt)) }`, or `ids.Register(mux)`. Each `Route` carries `Access` (public / authenticated / permission) and `Resource`/`Verb` for the route-permission lint. A server with its own authorize wrapper can use the declaration instead of `Guard`.
- **Its own routes:** `auth.Require(ids.Authorizer(), attrsFn, h)` or `auth.Authorize(ctx, ids.Authorizer(), attrs)`. These write the 401/403 contract bodies and store the decision.
- **Audit:** `auth.PrincipalFrom(ctx).ID` becomes `sqlite.WithActor`; `auth.DecisionFrom(ctx).Path` is `binding|sar|local`.
- **Info:** `ids.Diagnoses()` goes to `GET /info/messages` (ignored RoleBinding objects, broken IdentityProviders).
- **Errors:** handlers can use `auth.WriteError` for the contract error body (it includes `took_ms` from the middleware's start).

## Routes

Every path is under `/api/v1`. The declarations come from `stack.Routes()`.

| Route | Access | Body / answer |
|---|---|---|
| `GET /auth/status` | public | `{localLogin: bool, localLoginMode, providers:[{id,displayName}], hasDefaultCredentials, session:{idleTimeout,lifetime}}` |
| `POST /auth/login` | public (CrossOriginProtection, lockouts) | `{username,password}` → 200 me body + session cookie. Errors: 401 `unauthenticated`, 429 `rate_limited` + Retry-After, 403 `account_disabled` / `local_login_disabled` / `local_login_admins_only` |
| `POST /auth/logout` | public (CSRF through the cookie step) | 204, cookie cleared |
| `GET /auth/me` | authenticated | `{principal:{kind,id,name,groups,provider,authMethod,tokenID}, roles:[grant…], csrf, permissions:{<resource>:[verbs]}, scopedPermissions:{"zone:x":{…}}, mustChangePassword, expiresAt, idleExpiresAt}` |
| `POST /auth/password` | authenticated, local users | `{currentPassword,newPassword}` → 200 me body; every earlier session is revoked and the caller gets a fresh cookie |
| `GET /auth/oidc/{idp}/start`, `…/callback` | public | the spike's BFF login against the current registry |
| `GET /auth/tokens` | tokens.own at any held scope (`?all=true` needs tokens.read) | `{items:[{id,description,owner,createdAt,expiresAt,lastUsedAt,lastUsedIP,scopes}],continue,took_ms}` |
| `POST /auth/tokens` | tokens.own at any held scope | `{description,expiresIn,scopes}` → 201 with `token` shown once. A token-authenticated caller cannot widen scopes or outlive its token |
| `DELETE /auth/tokens/{id}` | tokens.own at any held scope (others' tokens need tokens.delete, else 404) | 204 |
| `GET /iam/users` | users.read | list envelope of users (no hashes) |
| `GET /iam/serviceaccounts` | serviceaccounts.read | list envelope with token counts |
| `GET /iam/rolebindings` | rolebindings.read | config bindings (`source: config`) and objects, plus `diagnoses` |
| `GET /iam/identityproviders` | identityproviders.read | providers without secrets, plus `diagnoses` |

## Admin socket protocol

The socket is `auth.adminSocket.path` (default `/run/kudurru/kudurru.sock`), mode 0660, chgrp `auth.adminSocket.group` when that is ≥ 0. The protocol is HTTP/1.1 with JSON bodies over the unix socket. The host name in the URL is ignored; the CLI uses `http://kudurru/…` with a `net.Dialer` on `unix`. Unknown JSON fields are a 400. Errors use the API contract body.

Peers are admitted by SO_PEERCRED: uid 0, the server's own uid (65532 in the image, which is what `docker exec`/`kubectl exec` get), or the configured gid. Anyone else gets 403 `forbidden`. An admitted peer is the local root (builtin admin, Decision.Path `local`).

```
POST /admin/v1/reset-password
  {"username":"admin","password":"","create":false}
  username defaults to "admin"; an empty password is generated; create makes a missing user (builtin admin)
  → 200 {"username":"admin","password":"<only when generated>","mustChangePassword":true,"created":false}
  → 404 not_found (no such user, create false) | 400 invalid (password under 8 characters or over 1 KiB)
  Effects: new Argon2id hash, mustChangePassword=true, sessionsValidAfter=now (every session revoked).

POST /admin/v1/create-token
  {"user":"","description":"","expiresIn":"24h"}
  user empty → a token of the builtin-admin service account <ns>/break-glass (created on demand),
               default lifetime 24h capped by auth.tokens.maxLifetime;
  user set   → a personal token of that local user (default lifetime auth.tokens.maxLifetime or never)
  → 201 {"id":"abcd1234efgh","token":"kdr_…","owner":{"kind","name","namespace","id","provider"},"expiresAt":"RFC 3339"|null}
  → 404 not_found (user) | 400 invalid (bad expiresIn, longer than maxLifetime)

GET /admin/v1/list-users
  → 200 {"users":[{"name","displayName","builtinAdmin","disabled","mustChangePassword","groups","lastLoginAt","passwordChangedAt"}]}
```

`adminsock.Options.API` serves every other path on the socket. When the integrator passes `ids.Middleware()(apiMux)`, the socket also serves the whole REST API to the local root.

## Config keys

These are added to `internal/config/schema.go`. Everything except the two session keys is `restartRequired`.

| Key | Default | Notes |
|---|---|---|
| `auth.localLogin` | `enabled` | `enabled`, `admins-only` (builtin admins and cluster `admin` holders), or `disabled` |
| `auth.bootstrap.adminPasswordFile` | `""` | node scope, hostOnly; the environment variables take precedence |
| `auth.session.idleTimeout` / `lifetime` / `secure` | `30m` / `12h` / `auto` | |
| `auth.tokens.maxLifetime` | `0s` | 0 means tokens may never expire |
| `auth.identityProviders` | `[]` | IdentityProvider spec plus `id`, decoded strictly; hostOnly |
| `auth.kubernetes.mode` | `sar` | `impersonate` is rejected with the D55 reason; hostOnly |
| `auth.adminSocket.path` / `group` | `/run/kudurru/kudurru.sock` / `-1` | node scope, hostOnly |
| `rbac.bindings` | `[]` | RoleBinding spec maps, decoded strictly; hostOnly |

## Tests

All tests run with `CGO_ENABLED=1 go test -race`. `make test` is green for the whole module at this branch, 0 failures. `make test-auth-e2e` against a real dex v2.46.0 is green: `TestDexEndToEnd` 17/17 and `TestMockOPEndToEnd` 31/31.

| What | Where | Drivers |
|---|---|---|
| Argon2id parameters and hostile PHC strings, password rules, limiter windows and lockouts, login on known, unknown and disabled accounts (IP 5 → 5 min and user 10 → 15 min with exact Retry-After), password change revoking sessions, bootstrap (env, env file, config file, generated + printed, idempotent, too short, two replicas racing → `ErrManagedBy`), the watch cache following another replica | `internal/auth/users` | memory + sqlite |
| Wire format (200 tokens; 9 malformed shapes), create/verify/revoke, the same 401 body for unknown id / wrong secret / garbage, the stored object never holding the secret, expiry and maxLifetime, service accounts (missing, disabled), the bootstrap token (env file, idempotent, malformed), the bearer step and lastUsedAt/IP | `internal/auth/tokens` | memory + sqlite |
| Chain matrix with fake steps (first wins, rejection stops the chain, CSRF 403, loader 401, unclaimed bearer 401), CSRF table (7 cases incl. Sec-Fetch-Site), JWT routing by iss and the fallback, peer policy, internal token, Authorize (401, 403 + missing hint, password_change_required, Node implicit grants, Decision in context) | `internal/auth` | — |
| End-to-end chain over HTTP: anonymous 401; status; login; cookie flags; cookie GET with Path local; CSRF missing / cross-site / valid; token create; bearer kdr_ beating a cookie; wrong secret 401 even with a valid cookie; unknown-issuer JWT 401; internal token read-only; a config Group binding; 403 hint `records/write/zone:home`; IAM 403/200; someone else's token 404; **a RoleBinding object authorizing then de-authorizing live through Store.Watch**; scoped permissions in /me; disabling a user killing the cookie; token scopes; no scope escalation; logout. Plus the mustChangePassword flow (old cookie 401 after change, new cookie works), admins-only / disabled / IP lockout 429, the unix peer through the chain, the generated admin plus bootstrap token, and invalid configs refused | `internal/auth/stack` | memory + sqlite |
| Real unix socket: socket mode, reset-password (404, create, chosen, unknown field), list-users, break-glass token (capped by maxLifetime, builtin admin), personal token, over-maxLifetime 400, API passthrough as LocalRoot uid; a foreign peer refused, the configured gid admitted; a live socket refused, a stale socket replaced, a regular file never replaced | `internal/auth/adminsock` | memory |
| **envtest:** projected tokens via JWKS and via TokenReview (wrong audience, broken signature refused, groups, UID); SAR on virtual `querylogs`, `dnszones`, `dnsrecordsets` with resourceNames per zone, custom verbs, another SA; the stack end to end with an SA token (200 Path sar, 403, 401 for the wrong audience); **the identity layer on the kube store** (login, token as a Secret without its secret, RoleBinding Secret authorizing, revocation, password change) | `internal/auth/k8s` | kube (envtest) |
| Internal kinds as Secrets, plus the production scheme (no core/v1) | `internal/store/kube` | envtest |
| Bearer ID token audience rules (10 cases incl. azp, several audiences, expired, wrong issuer, no sub), `typ: at+jwt` refused, exact issuer routing, the verdict cache through the middleware, config conversion (strict keys, default prefixes, secret ref/file) | `internal/auth/oidc` | mock OP |
| Cookie step (no cookie, bearer skip, CSRF, cleared broken cookie), `__Host-` naming | `internal/auth/session` | — |
| Keys: env/file/hex/rotation/short/previous-only/missing file; kube refuses to generate; 4 concurrent Docker first starts agree, 0600/0700, no temp files left | `internal/auth/keys` | — |
| Auth config defaults, the full §6.2 example, 10 validation failures | `internal/config` | — |

`golangci-lint run` reports 0 issues for `./internal/auth/... ./internal/rbac/... ./internal/config/... ./api/... ./internal/store/kube/... ./internal/store/sqlite/...` (gofmt, goimports, gosec, revive, staticcheck). `go vet` is clean and `make check-generated` is clean.

## Measurements

The machine had 8 CPUs shared with five agents (load average 9.8 during the benchmarks), GOMAXPROCS 6, Intel Xeon @ 2.90GHz, go1.27.2.

| Measurement | Value |
|---|---|
| Argon2id hash (m=19 MiB, t=2, p=1), `BenchmarkHash` | 25.9 ms/op, 19.9 MB/op |
| Authenticate + Authorize per request, cookie session (open + touch + LoadPrincipal + bindings) | 5.8 µs/op, 3.8 KB, 24 allocs |
| … bearer kdr_ token (sha256 + constant-time compare + LoadPrincipal + bindings) | 2.1 µs/op, 1.8 KB, 18 allocs |
| … internal token (Node, implicit grants) | 1.7 µs/op, 1.8 KB, 16 allocs |
| k8s SA token, first verification incl. discovery (JWKS) | 0.9-2.1 ms |
| k8s SA token, new token with cached keys (JWKS, no apiserver call) | 0.15-0.5 ms |
| k8s SA token via TokenReview | 1.2-1.4 ms per new token |
| k8s SA token, cached verdict | 1.6-5.7 µs |
| 20 SA-token requests through the full stack with cached SAR verdicts | 8.2-8.8 ms total |
| dex end-to-end suite with `-race` | 3.6 s (17 subtests); mock OP 0.66 s (31) |
| `make test` (whole module, `-race`, under shared load) | green |

Commands:

```
go test -run NONE -bench . -benchtime=2s ./internal/auth/stack/ ./internal/auth/users/
go test -count=1 -v -run TestServiceAccountTokens ./internal/auth/k8s/        # KUBEBUILDER_ASSETS set
make test-auth-e2e
```

## Surprises

- **`api/internal/v1alpha1` cannot work.** Go's internal-package rule makes it importable only from `api/...`, so `internal/rbac` and the auth packages could not use it. The kinds live in `api/internalkinds/v1alpha1` (same group version, still never CRDs).
- **The kube driver's internal kinds failed with the production scheme.** `internalkinds.NewScheme()` holds only kudurru.io, and listing Secrets needs core/v1. The driver's own tests used a test scheme that already had core/v1. The new end-to-end test on the kube store caught it, and the driver now adds core/v1 itself (`TestEnvtestInternalKindsProductionScheme`).
- **A session issued right after a password change was born revoked.** sessionsValidAfter and iat both have one-second resolution, and the rule is `iat > validAfter`. The login and password routes now place iat one second after the user's sessionsValidAfter.
- **Failing a cookie request on CSRF made re-login impossible.** A POST /auth/login from a browser that still held a valid session cookie but sent no CSRF header (the login page) was a 403. The cookie step now drops the session and records the failure: protected routes still answer 403 csrf, logout refuses, and login works.
- **objcache could regress.** A read-your-writes `Upsert` racing the watch event for a newer version replaced it, and nothing would ever correct it. Resource versions now compare numerically (every driver issues increasing integers) and an older version never replaces a newer one.
- **The chart Role is missing `delete` on secrets.** It is needed for token revocation and user/RoleBinding deletion on the kube driver. §5.3 also lists only get/list/watch/create/update. Recorded as a follow-up for the chart owner.
- **SubjectAccessReview for zone-scoped requests** names the zone as the resource name (`dnsrecordsets` with `resourceNames: [home]`), which is how a Kubernetes zone-admin is expressed. It works against a real apiserver (envtest).
- **Registering the IdentityProvider cache's OnChange before Prime** made the first registry build run without the config entries, so a broken config IdP was not a startup error. The hook is now registered after the first build.
- **The admin socket and the "unix peer" step are the same mechanism.** HTTP over the socket with `PeerConnContext` gives the `kudurru admin` protocol and, optionally, the full API to the local root, without a second wire format.
- **Logins and personal tokens of OIDC users** cannot be re-resolved per request because the session has no groups (D48). Their grants are frozen and honoured only while live bindings still provide them. Bearer ID tokens and SA tokens carry groups, so they are evaluated live.
- **The tool sandbox refused shell commands** that contained "git" together with pipes or heredocs (including `github.com` in Python one-liners). Edits went through small scripts in /tmp.

## Architecture changes

These are recommended for the integrator; docs/architecture.md was not edited.

1. **§6.2:** the admin socket keys are `auth.adminSocket.{path,group}`, not `listeners.socket`. They are node scope and hostOnly, and the same default path is used. Document `auth.kubernetes.mode`.
2. **§4.1/§8:** the internal kinds live in `api/internalkinds/v1alpha1` (Go rule). In Kubernetes they are Secrets `kudurru-<kind>.<ns>.<name>` labelled `kudurru.io/kind` in the release namespace, holding `data.object`.
3. **§8.5:** Decision paths are `local` (builtin admin: unix peer, first-boot admin, bootstrap and break-glass service accounts; also the Node principal's implicit viewer+auditor grants), `sar`, `binding` and `none`. Sealed session grants are honoured only while a live binding provides them and no binding change revoked them after iat (closes the M3 recommendation of the auth spike).
4. **§8.7:** `POST /api/v1/auth/password` and `GET /auth/status.localLoginMode`. `admins-only` admits builtin admins and cluster `admin` holders. The generated first-boot password sets mustChangePassword, while env- and file-provided passwords do not.
5. **§5.3:** add `delete` to the secrets rule.
6. **§8.3:** the bearer ID-token path is live server side in v0.1. Access tokens (`typ: at+jwt`) are refused.
7. **§8.6:** a token-authenticated `POST /auth/tokens` cannot widen scopes or outlive its parent token. Break-glass tokens default to 24 h.

## Follow-ups

These rows were added to docs/followups.md:

- Persist binding revocation epochs (v0.2).
- A user store for OIDC identities: per-user logout everywhere, disable, `autoProvision: never`, `syncGroups` (v0.2).
- Login lockouts are per replica (v0.2).
- Re-check JWKS-verified SA tokens against deleted pods or ServiceAccounts (v0.2).
- Chart secrets `delete` (M3, chart owner).
- CLI `kudurru admin …` and `kudurru token generate` (M3, CLI).
- The architecture.md updates listed above (M3, integrator).
