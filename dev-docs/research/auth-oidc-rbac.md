> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [dev-docs/design-record.md](../design-record.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# kudurru research: authentication, OIDC, and RBAC

Key: `auth-oidc-rbac` — researched 2026-10-10. Primary sources were shallow clones under
`/tmp/kudurru-research/auth-oidc-rbac/<repo>` (commit hashes below) plus pkg.go.dev, rfc-editor.org,
kubernetes.io and vendor docs. Anything not read directly is marked **UNVERIFIED**.

## 0. TL;DR (decisions this report recommends)

| Topic | Recommendation |
|---|---|
| OIDC client library | `github.com/coreos/go-oidc/v3` (discovery + ID-token verification + JWKS cache) on top of `golang.org/x/oauth2` (auth-code, PKCE, device flow, refresh). Do not take `zitadel/oidc` as a hard dependency; borrow its `rp/cli` local-callback pattern. |
| Web UI login | Backend-for-Frontend (RFC 10017 / BCP 212): server runs the authorization-code + PKCE flow as a *confidential* client, keeps IdP tokens server-side, and gives the SPA an opaque `HttpOnly; Secure; SameSite=Lax` session cookie. No tokens in the browser. |
| CSRF | Go 1.25 `net/http.CrossOriginProtection` (Sec-Fetch-Site/Origin) + `SameSite=Lax` cookie + a per-session CSRF header for cookie-authenticated mutating API calls (Pi-hole style). Bearer-token requests skip CSRF. |
| CLI login | Both RFC 8628 device flow (`kudurru login --device`) and local loopback callback with PKCE (default, like `kubectl oidc-login`/`gcloud`). CLI presents the IdP **ID token** as `Authorization: Bearer` and refreshes with the cached refresh token (OS keyring). The server verifies against the issuer's JWKS and re-applies claim mapping on every call. |
| Claims -> roles | Kubernetes-style structured mapping: `claimMappings.username/groups` with `claim`+`prefix` *or* a CEL `expression`, `claimValidationRules`, plus an explicit `groupMap` (remote group -> kudurru group). Engine: `github.com/google/cel-go` (Apache-2.0). |
| Local accounts | Argon2id (OWASP m=19 MiB,t=2,p=1), per-IP + per-user login rate limiting, bootstrap `admin` whose password comes from `KUDURRU_ADMIN_PASSWORD[_FILE]` or is generated and printed once; `hasDefaultCredentials` nag; break-glass `?local=1` login page and a local-socket CLI reset. |
| API tokens | Grafana-style **service accounts** that own N tokens. Token format `kdr_<12-char id>_<43-char secret>`; store `sha256(secret)` (Headscale rationale); optional expiry, scopes (role bindings) attached to the *service account*, shown once, revocable. Also "personal" tokens bound to a user with a subset of the user's bindings. |
| Kubernetes identity | Accept projected ServiceAccount tokens as bearer: verify via the API server's OIDC discovery/JWKS (stable since k8s 1.21) with `aud: kudurru`, falling back to `TokenReview` when discovery is not reachable. Map `system:serviceaccount:<ns>:<name>` to an identity and bind it with the same RoleBinding objects. |
| RBAC engine | Hand-rolled, explicit model (resources x verbs x scope) stored as role bindings; Casbin is a viable embedded alternative but adds a policy DSL without buying much for a ~12-resource domain. OpenFGA/SpiceDB are network services and are out of scope. |
| Roles | Built-in: `viewer`, `operator`, `zone-admin` (scoped to one or more zones), `admin`, plus `auditor` (query-log reader) because query logs are privacy-sensitive. Custom roles allowed later; bindings are `(subject, role, scope)` where scope is `cluster` or `zone:<name>`. |
| Storage | Same relational model in both modes: SQLite/Postgres tables in Docker; in Kubernetes the *declarative* parts (IdentityProvider, RoleBinding, ServiceAccount) are CRDs reconciled into the same tables, while *runtime* parts (sessions, audit, token hashes) stay in the DB / k8s Secrets. |

## 1. Sources (versions, licenses, dates)

| Project / library | Clone commit or version | Date | License | Notes |
|---|---|---|---|---|
| Technitium DnsServer | `70495af` (v15.6 changelog) | 2026-10-03 | GPL-3.0 | C#; auth in `DnsServerCore/Auth/*`, `WebServiceAuthApi.cs`, `APIDOCS.md` |
| Gravity (BeryJu) | `b873d1f` | 2026-10-09 | GPL-3.0 | Go 1.26.6; `go-oidc/v3 v3.21.0`, `x/oauth2 v0.37.0`, `gorilla/sessions v1.4.0`, `api7/etcdstore` |
| AdGuard Home | `8dedd32` | 2026-10-08 | GPL-3.0 | Go 1.27.1; `internal/home/auth*.go`, `internal/aghuser` |
| Pi-hole FTL (v6 API) | `0bf029b` | 2026-09-19 | EUPL-1.2 (verified from repo LICENSE) | C; `src/api/auth.c`, `2fa.c`, `config/password.c`, OpenAPI `src/api/docs/content/specs/auth.yaml` |
| PowerDNS-Admin | `cb09bb5` | 2026-08-31 | MIT | Python/Flask; `powerdnsadmin/models/*`, `decorators.py` |
| coreos/go-oidc | v3.21.0 (`c914bd3`) | 2026-09-01 | Apache-2.0 | module `github.com/coreos/go-oidc/v3`, go 1.25; prior: v3.20.0 2026-07-08, v3.17.0 2025-11-21 |
| zitadel/oidc | v3.51.14 (`5ac478e`) | 2026-10-09 | Apache-2.0 | module `/v3`, go 1.26; `v4.0.0-next.4` pre-release 2026-07-30 (pkg.go.dev) |
| golang.org/x/oauth2 | v0.37.0 | 2026 (pkg.go.dev) | BSD-3-Clause | Device flow + PKCE helpers since v0.13.0 |
| casbin/casbin | v3.11.0 | 2026-06-23 | Apache-2.0 | module `github.com/casbin/casbin/v3`; v3.0.0 2025-12-09 |
| juanfont/headscale | `a8d6f5b` | 2026-10-09 | BSD-3-Clause | go 1.27; `hscontrol/oidc.go`, `hscontrol/db/secret.go` |
| int128/kubelogin | `19acbea` | 2026-10-09 | Apache-2.0 | `pkg/oidc/client/{authcode,devicecode}.go`, `docs/usage.md` |
| google/cel-go | v0.31.0 shown on pkg.go.dev (moving to `github.com/cel-expr/cel-go` 2026-06-16) | — | Apache-2.0 | CEL engine (used by Kubernetes) |
| expr-lang/expr | v1.17.8 | 2026-02-14 | MIT | lighter alternative expression engine |
| alexedwards/scs | v2.9.0 (page shown; newer may exist) | 2025-04-17 | MIT | server-side sessions; stores: postgres, pgx, mysql, sqlite3, redis, etcd, bbolt, memstore, ... |
| RFC 9700 | BCP 240 "Best Current Practice for OAuth 2.0 Security" | 2025 | — | updates 6749/6750/6819 |
| RFC 10017 | BCP 212 "OAuth 2.0 for Browser-Based Applications" (Parecki, De Ryck, Waite) | 2026 (datatracker title says Aug 2026) | — | BFF pattern |
| RFC 10027 | BCP 247 "Best Current Practice for Security of Cross-Device Flows" | 2026 (Aug per datatracker) | — | device-flow phishing (CDCP) |
| Kubernetes docs | Structured `AuthenticationConfiguration` stable since v1.34; SA issuer discovery stable since v1.21 | — | — | kubernetes.io |
| Go stdlib | `net/http.CrossOriginProtection` added Go 1.25.0 | — | BSD-3 | pkg.go.dev |

## 2. Survey of reference projects

### 2.1 Technitium DNS Server — the most complete model (document precisely)

Files: `DnsServerCore/Auth/{AuthManager,Permission,Group,User,UserSession,LdapAuthProvider}.cs`,
`DnsServerCore/WebServiceAuthApi.cs`, `DnsServerCore/DnsWebService.cs` (OIDC middleware), `APIDOCS.md`.

**Permission sections** (`enum PermissionSection : byte`): `Dashboard=1, Zones=2, Cache=3, Allowed=4, Blocked=5,
Apps=6, DnsClient=7, Settings=8, DhcpServer=9, Administration=10, Logs=11`.

**Permission flags** (`[Flags] enum PermissionFlag : byte`): `View=1, Modify=2, Delete=4` (`ViewModify=3`,
`ViewModifyDelete=7`).

**Permission object** per section:
```
Permission {
  section: PermissionSection
  subItemName: string?                     // e.g. a zone name under Zones
  userPermissions:  map[User]  -> PermissionFlag
  groupPermissions: map[Group] -> PermissionFlag
  subItemPermissions: map[lowercase name] -> Permission   // nested, same shape
}
```
Checks: `IsPermitted(section, user, flag)` and `IsPermitted(section, subItem, user, flag)`; a user is permitted
if either a direct user entry or any of its groups carries the flag. Zone-level ACL = `Zones` section + sub-item
`example.com`. Observed semantics in `WebServiceZonesApi.cs`:
- listing zones needs `Zones:View`, and each zone is shown only if the caller has sub-item `View` on it
  (line ~1495);
- creating a zone needs `Zones:Modify`; on creation the server grants `ViewModifyDelete` on the new sub-item to
  the creating user, the `Administrators` group and the `DNS Administrators` group (lines 1660-1662);
- `GET /api/zones/permissions/get` needs `Zones:Modify` + `Zone:View`; `.../set` needs `Zones:Modify` +
  `Zone:Delete`; the wire format is pipe tables `user1|true|true|true|user2|true|false|false`.

**Groups**: `Administrators`, `Everyone`, `DNS Administrators`, `DHCP Administrators` are built in and cannot be
renamed. Default section grants (from `/api/admin/permissions/list`): `Everyone` has `View` on Dashboard and Zones;
`DNS Administrators` has `ViewModifyDelete` on Zones; `DHCP Administrators` has `View` on Zones, etc.

**Users** (`User.cs`): `UserType {Local=0, RemoteSSO=1, RemoteLDAP=2}`; `UserPasswordHashType {OldScheme=1
(HMAC-SHA256 of username keyed by password), PBKDF2_SHA256=2}`; PBKDF2 iterations 100 000..1 000 000, default
100 000 (v15.6 fixed a DoS where a user could set arbitrary iterations); TOTP (`AuthenticatorKeyUri`); `Disabled`;
per-user `SessionTimeoutSeconds` 0..604800 (default 30 min; 0 = never); previous/recent login time + IP; group
membership list. SSO users cannot log in with a password (`case UserType.RemoteSSO: ThrowAuthFailed`).

**Sessions / tokens** (`UserSession.cs`): `UserSessionType {Standard=1, ApiToken=2, ClusterApiToken=3,
SingleUse=4}`; token is 32 random bytes hex (64 chars); `Standard` expires `lastSeen + user.SessionTimeout`;
`ApiToken`/`ClusterApiToken` never expire; each session records `tokenName`, `lastSeen`, `lastSeenRemoteAddress`,
`lastSeenUserAgent`. All sessions (including API tokens) are persisted in the auth config file and listed via
`/api/admin/sessions/list` with a `partialToken` (first 16 hex chars) used to delete. **Security note**: v15.6
(2026-10-03) fixed a "session token prefix oracle" in the delete-sessions API that let an authenticated user
recover full tokens — a cautionary tale for prefix-based lookups: compare whole hashes, never prefixes.

**API tokens**: `POST /api/user/createToken` (self, needs user+pass[+totp] or a session) or
`/api/admin/sessions/createToken?user=&tokenName=` (admin, `Administration:Modify`). A token "allows access to API
calls with the same privileges as that of the user account" but "cannot be used to change the user's password, or
update the user profile details". Technitium's own advice: create a dedicated low-privilege user per automation.
`createSingleUseToken` yields a token that dies after one call (used for download links).

**HTTP API auth** (v15+): `Authorization: Bearer <token>`; legacy `token` query/form param still accepted.
Response envelope `status: ok | error | invalid-token | 2fa-required`.

**Login hardening** (`AuthManager.cs`): failures counted per client *network* (IPv4 /32, IPv6 /64);
`MAX_LOGIN_ATTEMPTS = 5` then block for `BLOCK_NETWORK_INTERVAL = 5 min`; every failed login sleeps 1 s;
`HasDefaultCredentials()` surfaces `admin/admin` in `/api/user/status` so the UI nags. v15.6 also fixed log
injection via the `user` parameter — sanitize usernames before logging.

**OIDC SSO** (added v15.0, 2026-04-25; `DnsWebService.cs` uses ASP.NET Core `AddOpenIdConnect`, `ResponseType =
code`, claim actions for `sub,email,preferred_username,upn,nickname,name`). Config (API `/api/admin/sso/get|set`,
env `DNS_SERVER_SSO_*`):
```
ssoEnabled, ssoAuthority (issuer), ssoClientId, ssoClientSecret (+ *_FILE env), ssoMetadataAddress (custom
discovery URL), ssoScopes (openid,profile mandatory; email optional), ssoAllowSignup,
ssoAllowSignupOnlyForMappedUsers (default true), ssoGroupMap "RemoteGroup:LocalGroup,...", ssoManagedGroups
```
`SsoLoginFinalizeAsync` logic (worth copying almost verbatim):
1. identifier = `sub` ?? `nameidentifier` ?? MS `objectidentifier`; abort if missing (log the claim *names* only).
2. username candidates: `preferred_username` ?? `upn` ?? `nickname`; display name: `name` ?? given name; email.
3. remote groups = all `groups` / `roles` / `ClaimTypes.Role` claim values.
4. lookup user by `ssoIdentifier`; if absent: refuse unless `ssoAllowSignup`; if `OnlyForMappedUsers`, require at
   least one remote group present in `ssoGroupMap`; pick local username = email if valid & free, else username.
5. existing user: refuse if `Disabled`; sync username/display name when they changed at the IdP.
6. group sync: local groups := mapped(remote groups) ∪ {Everyone}; `user.SyncGroups(...)` (replaces, not merges)
   — with `ssoManagedGroups` the UI refuses manual group edits for SSO users.
7. create `Standard` session; on cluster secondaries push the SSO user to the primary via the cluster API token.
8. hand-off to the SPA: `Set-Cookie: token=<session>; Max-Age=120` then redirect `/`; `www/js/auth.js` reads the
   cookie, deletes it and **stores the token in `localStorage`** (all subsequent calls are header-bearer, so no
   CSRF concern but XSS-exfiltrable; the cookie is set without HttpOnly on purpose).

**LDAP** mirrors SSO: `LdapAllowSignup`, `LdapAllowSignupOnlyForMappedUsers`, `LdapGroupMap`, `LdapManagedGroups`,
bind DN/password, user search filter, group attribute.

**Clustering**: a `ClusterApiToken` session type is used node-to-node; `/api/admin/sso/users/create|set` are
cluster-only endpoints to replicate SSO user records (users/groups/permissions live in a binary config file that is
transferred to secondaries).

Weak spots to avoid: binary config file (not a DB), prefix-based session deletion, token in localStorage, PBKDF2
instead of Argon2id, no PKCE visible in the SSO config (ASP.NET handler defaults to PKCE for code flow —
**UNVERIFIED** here), no per-token scopes, no audit trail beyond text logs.

### 2.2 Gravity (BeryJu) — simplest Go implementation, etcd-native

Files: `pkg/roles/api/auth/*.go`, `pkg/roles/api/role.go`, `protobuf/role_api_{user,token}.proto`,
`docs/content/docs/api/{permissions,role_config}.md`.

- **Users** (`message User { username, password (bcrypt), repeated Permission permissions }`) stored at etcd key
  `/<prefix>/role/api/users/<username>`.
- **Permissions** are HTTP path globs + methods: `{ "path": "/api/v1/dns/zones", "methods": ["get"] }`;
  `checkPermission` picks the longest matching path (`/*` wildcard suffix) and allows if the method matches or is
  `*`. Admin = `{"path":"/*","methods":["*"]}`. No notion of zone-scoped rights; a "zone editor" would need per-zone
  URL patterns and the API would need zone names in paths.
- **Sessions**: `gorilla/sessions` with `api7/etcdstore` — session blobs live in etcd under `role/api/sessions`,
  encrypted/authenticated with `cookieSecret` from the API role config (shared cluster-wide, so any node can serve
  any session). `Options.MaxAge = sessionDuration` (default 24h). The `sessionWriter` only saves when a `dirty`
  flag is set. Cookie `Secure`/`SameSite` are gorilla defaults (**UNVERIFIED** whether Secure is set).
- **Local login**: `POST /api/v1/auth/login {username,password}`; bcrypt compare with a dummy compare on unknown
  user (timing-equalising). No rate limiting observed.
- **API tokens**: `POST /api/v1/auth/tokens?username=` returns `key` = base64(64 random bytes); stored
  **in plaintext as the etcd key** `role/api/tokens/<key>` -> `{key, username}`; `checkStaticToken` looks the bearer
  value up directly. Listing tokens returns the full keys. Bootstrap: `ADMIN_PASSWORD` / `ADMIN_TOKEN` env on first
  start, else a random password printed to stdout.
- **OIDC** (`method_oidc.go`): `go-oidc` discovery, `oauth2.Config`, state = 32 random bytes kept in the session,
  code exchange, `id_token` verification, claims `{email}`; the resulting session user is
  `{Username: email, Permissions: ["/*" all methods]}` — **every OIDC login is a full admin**; no group/claim
  mapping, no PKCE, no nonce. `redirectURL` supports `$INSTANCE_IDENTIFIER` / `$INSTANCE_IP` placeholders because
  every node has its own URL. Since 0.19, a bearer JWT signed by the issuer is accepted; `tokenUsernameField`
  selects the claim used to look up a *pre-existing* local user (good pattern: JWT bearer for automation).
- UI auto-starts SSO when OIDC is configured; `?local` query param forces the local form (break-glass).
- Config shape (`role_config.md`): `oidc: {clientID, clientSecret, issuer, redirectURL, scopes, tokenUsernameField}`
  stored in etcd role config (secret at rest in etcd).

### 2.3 AdGuard Home — basic auth + cookie, no roles

- Users in YAML: `users: [{name, password: <bcrypt, htpasswd -B>}]`; `auth_attempts: 5`, `block_auth_min: 15`
  (`authRateLimiter` keyed by remote IP; failed entries live 1 min, block applies after `maxAttempts`), `trusted_proxies`
  for real-IP via `CF-Connecting-IP`/`True-Client-IP`/`X-Real-IP`/`X-Forwarded-For`.
- Login `POST /control/login {name,password}` -> cookie `agh_session=<hex 16 bytes>; Path=/; HttpOnly; SameSite=Lax;
  Expires=+365d` (no `Secure` attribute). Sessions persisted in bbolt (`sessions.db`) with `session_ttl` (default 30 d);
  `Cache-Control: no-store` on the login response. HTTP Basic auth is also accepted on every request (rate-limited).
- No user roles, no OIDC/SSO, no API tokens (basic auth with the user password is the API credential); GL.iNet build
  trusts a router-issued token file. Third parties front it with Pomerium/Authelia for SSO.

### 2.4 Pi-hole v6 (FTL) — one password, but a careful session design

- Single password: `webserver.api.pwhash` = PHC string `$BALLOON-SHA256$v=1$s=1024,t=32$<salt>$<hash>` (Balloon
  hashing via nettle, 16-byte salt); `webserver.api.app_pwhash` = separate 32-byte random **app password** (shown once,
  usable instead of password+TOTP, creating one invalidates sessions); `webserver.api.totp_secret` (write-only);
  `webserver.api.cli_pw` = per-boot random password written to `/etc/pihole/cli_pw` for the local `pihole` CLI — its
  sessions "cannot modify the Pi-hole configuration" (a read-mostly local automation identity);
  `webserver.api.max_sessions` default 16; `webserver.session.timeout` default 1800 s, extended on every request.
- `POST /api/auth {password[,totp]}` -> `{session:{valid,totp,sid,csrf,validity}}`; SID accepted via query `sid`, JSON
  `sid`, header `X-FTL-SID`, or cookie `sid` (`Set-Cookie: sid=...; SameSite=Lax; Path=/; Max-Age=N; HttpOnly[; Secure]`).
  Cookie-authenticated **API** calls must also send the CSRF token; the FTL source at this commit reads
  `X-CSRF-TOKEN` while docs.pi-hole.net says `X-FTL-CSRF` (discrepancy — treat the header name as **UNVERIFIED**).
  Constant-time `memeql_sec` comparisons; sessions record remote addr, UA, X-Forwarded-For, TLS-at-login and a
  `tls.mixed` flag. `DELETE /api/auth` logs out; `GET/DELETE /api/auth/sessions[/{id}]` list/kill.
- Rate limiting: password check returns `PASSWORD_RATE_LIMITED` -> 429 `rate_limiting`; TOTP verification allows
  one attempt per second and tracks the last accepted RFC 6238 counter so a code can never be replayed.
- Docker bootstrap: `FTLCONF_webserver_api_password` env.
- No users, groups, roles, or SSO.

### 2.5 PowerDNS-Admin — roles + per-zone/account ACL + API keys

- `Role {id,name,description}` with fixed names `Administrator`, `Operator`, `User` enforced by decorators
  (`operator_role_required`, `admin_role_required`, ...). `User {username, password (bcrypt), email, otp_secret,
  otp_last_used, confirmed, role_id}`.
- Scoping: `DomainUser(domain_id,user_id)` grants a plain `User` rights on specific zones; `Account {name,...}` groups
  zones (`Domain.account_id`) and `AccountUser(account_id,user_id)` grants rights on all zones of an account —
  i.e. a two-level scope (zone, zone-group) that kudurru should reproduce as `zone:<name>` and a `tag`/`view` scope.
- `ApiKey {key (bcrypt hash), description, role_id, domains[], accounts[]}` — the key carries its **own role** and an
  explicit allow-list of zones/accounts (closest existing example of scoped tokens). API auth: `X-API-Key` header, or
  HTTP Basic with a user account (operator or above).
- Auth sources: local, LDAP/AD, SAML, OAuth (Google/GitHub/Azure/OIDC) with `autoprovisioning` driven by a configurable
  claim attribute (`apply_autoprovisioning(user, auth_method)` in `routes/index.py`; details **UNVERIFIED**).
- `History {msg, detail (JSON text), created_by, created_on, domain_id}` = activity log per zone (change auditing).

### 2.6 dnsdist (load balancer kudurru will embed/drive)

- Built-in webserver: Lua `webserver("127.0.0.1:8083"); setWebserverConfig({password=..., apiKey=..., acl=...,
  customHeaders=..., apiRequiresAuthentication=true, statsRequireAuthentication=true,
  dashboardRequiresAuthentication=true})`; YAML (dnsdist 2.x) `webserver: {listen_addresses, password, api_key, acl,
  custom_headers}`.
- API calls authenticate with the `X-API-Key` header (Basic auth is *not* allowed on `/api/*` other than
  `/api/v1/servers/localhost`; CVE-2017-7557 was exactly an API-key/Basic confusion). `hashPassword()` produces salted
  hashes for `password`/`apiKey` (dnsdist ≥ 1.7 warns on plaintext). Default ACL is loopback only; writable config
  requires `setAPIWritable()`.
- Implication: kudurru must manage one *dnsdist API key secret* per dnsdist instance as an **integration credential**
  (a `Secret` in k8s, a file in Docker), never a user credential.

### 2.7 Headscale — clean OIDC config and API-key hygiene

- `oidc:` config: `issuer, client_id, client_secret | client_secret_path ("${CREDENTIALS_DIRECTORY}/oidc_client_secret"
  — resolves env vars, systemd LoadCredential friendly), scope [openid profile email], extra_params {domain_hint},
  allowed_domains, allowed_users (emails), allowed_groups ("/headscale" — Keycloak path style), email_verified_required
  (default true), use_expiry_from_token, pkce {enabled, method: S256|plain}, only_start_if_oidc_is_available`.
- Claims struct: `sub, iss, name, groups (FlexibleStringSlice: string or array), email, email_verified
  (FlexibleBoolean: bool or "true"), picture, preferred_username`; stable identity key =
  `Identifier() = iss + "/" + sub` (never email). `doOIDCAuthorization` checks groups first, then (only if email is
  trusted) domains/users. The userinfo endpoint is queried and its claims override ID-token claims when `sub` matches.
  State is a random `authID` kept in a server-side registration cache; PKCE verifier stored alongside it.
- API keys (`hscontrol/db/secret.go`): string `hskey-api-<12 hex id>-<64 hex secret>`; DB stores the public identifier
  (indexed) and `$sha256$<hex>` of the secret. Rationale in code: 256-bit random secrets need no password stretching
  (NIST SP 800-63B-4 §3.1.2.2 / RFC 6819 §5.1.4.1.3); bcrypt/argon2 would only add CPU cost on unauthenticated
  endpoints (they bound legacy verifications with a semaphore). Unknown id and wrong secret return the same error.
  Keys have optional `Expiration`, can be expired (revoked) and are logged only as masked prefix.
- CLI: talks gRPC over a **unix socket with no auth** (`unix_socket: /var/run/headscale/headscale.sock`, mode 0770) or
  remotely with `HEADSCALE_CLI_API_KEY` as `Authorization: Bearer`.

### 2.8 Grafana — role mapping expressions and service accounts

- Generic OAuth: `role_attribute_path` is a **JMESPath** expression evaluated against ID token, then userinfo, then
  access token; result must be `None|Viewer|Editor|Admin|GrafanaAdmin`. Example
  `contains(groups[*], 'admin') && 'Admin' || contains(groups[*], 'editor') && 'Editor' || 'Viewer'`.
  `role_attribute_strict=true` denies login when no valid role results; `allow_assign_grafana_admin`;
  `org_attribute_path` (must yield an array) + `org_mapping = "<ExternalName>:<OrgIdOrName>:<Role> ..."` (`*`
  wildcard, no partial globs); highest role wins when both apply; `skip_org_role_sync` freezes roles after first login;
  `groups_attribute_path` + `allowed_groups`; `allowed_domains`; `use_pkce`; `use_refresh_token` (+ `offline_access`
  scope); `signout_redirect_url`. All settings overridable by env vars.
- Service accounts: non-user principals with an org role (`None/Viewer/Editor/Admin`), owning multiple tokens
  (random strings, optional expiry, `token_expiration_day_limit` cap), replacing legacy API keys; users/teams can be
  granted Edit/Admin on a specific service account.

### 2.9 Kubernetes structured authentication config — the gold standard for claim mapping

`apiserver.config.k8s.io/v1 AuthenticationConfiguration` (stable since **v1.34**, first in 1.29; up to 64 `jwt`
authenticators, issuer URLs unique):
```yaml
jwt:
- issuer:
    url: https://example.com            # must be unique
    discoveryURL: https://discovery.example.com/.well-known/openid-configuration   # optional override
    certificateAuthority: <PEM>
    audiences: [my-app, my-other-app]
    audienceMatchPolicy: MatchAny
  claimValidationRules:
  - claim: hd            # simple form
    requiredValue: example.com
  - expression: 'claims.hd == "example.com"'   # CEL form
    message: the hd claim must be set to example.com
  claimMappings:
    username: { claim: "sub", prefix: "" }     # or expression: 'claims.username + ":external-user"'
    groups:   { claim: "groups", prefix: "" }  # or expression: 'claims.roles.split(",")'
    uid:      { claim: "sub" }
    extra:
    - key: 'example.com/tenant'
      valueExpression: 'claims.tenant'
  userValidationRules:
  - expression: "!user.username.startsWith('system:')"
    message: 'username cannot used reserved system: prefix'
```
CEL variables: `claims` (map of the token payload) in claim rules/mappings, `user` in user rules. Distributed claims
are not available to CEL. Legacy flags: `--oidc-username-claim` (default `sub`), `--oidc-username-prefix`,
`--oidc-groups-claim`, `--oidc-groups-prefix`, `--oidc-required-claim key=value`, `--oidc-signing-algs`.

**TokenReview** (`authentication.k8s.io/v1`): request `spec: {token, audiences: [...]}`; response `status:
{authenticated, user: {username, uid, groups, extra}, audiences: [intersection]}` or `{authenticated:false, error}`.
Audience-aware clients must set `spec.audiences` and verify a compatible value comes back in `status.audiences`
(the MinIO operator CVE-2025-32963 fix is the canonical example). The reviewer needs RBAC `create` on
`tokenreviews.authentication.k8s.io`.

**Service account issuer discovery** (stable since 1.21): API server serves `/.well-known/openid-configuration` and
`/openid/v1/jwks`; `--service-account-issuer` must be an `https` URL; `--service-account-jwks-uri` can point at a
public mirror; ClusterRole `system:service-account-issuer-discovery` is bound to `system:serviceaccounts` by default
(admins may bind `system:authenticated`/`system:unauthenticated`). Projected tokens:
```yaml
volumes:
- name: kudurru-token
  projected:
    sources:
    - serviceAccountToken: { path: token, expirationSeconds: 3600, audience: kudurru }
```
Claims: `iss`, `sub: system:serviceaccount:<ns>:<sa>`, `aud`, `exp/iat/nbf`, `kubernetes.io: {namespace,
serviceaccount{name,uid}, pod{name,uid}}`.

### 2.10 Authentik (as an RBAC model, briefly)

Roles bundle permissions; roles are assigned to **groups**; permissions can be *global* (model-level) or
*object-level* (a specific instance); "initial permissions" presets what the creator gets on objects they create
(exactly what Technitium does implicitly for zone creators). Details beyond the overview page **UNVERIFIED**.

### 2.11 CLI login patterns in the wild

- `kubelogin` (kubectl oidc-login): grant types `auto|authcode|authcode-keyboard|password|device-code|client-credentials`;
  authcode starts a loopback server on `127.0.0.1:8000` then `:18000`, redirect URI `http://localhost:<port>`, PKCE
  method negotiated from discovery `code_challenge_methods_supported`, `nonce` set, optional `--local-server-cert`,
  `--open-url-after-authentication`, `--skip-open-browser`; token cache on disk (`~/.kube/cache/oidc-login`) or OS
  keyring (`zalando/go-keyring`) or none; refresh via refresh token.
- `zitadel/oidc` ships `rp/cli.CodeFlow(ctx, rp, callbackPath, port, stateFn)` + `OpenBrowser` — a 40-line reference
  for the loopback flow.
- `gh auth login` and `az login --use-device-code` use RFC 8628; `gcloud auth login` uses loopback+PKCE with a
  `--no-browser` remote fallback (**UNVERIFIED** details, from memory).
- RFC 8628 essentials: `POST device_authorization_endpoint {client_id, scope}` -> `{device_code, user_code,
  verification_uri, verification_uri_complete?, expires_in, interval}`; poll `token_endpoint` with
  `grant_type=urn:ietf:params:oauth:grant-type:device_code`; errors `authorization_pending`, `slow_down` (+5 s),
  `access_denied`, `expired_token`. RFC 10027 (BCP 247) warns about cross-device consent phishing: show the
  user code and the client name prominently, bind codes to short expiry, prefer `verification_uri_complete` only when
  the browser opens on the same machine, and prefer the loopback flow when a browser is local.

## 3. OIDC library comparison

| Capability | `coreos/go-oidc/v3` v3.21.0 | `zitadel/oidc/v3` v3.51.14 | `golang.org/x/oauth2` v0.37.0 |
|---|---|---|---|
| Role | RP only (ID-token verification, discovery, userinfo, JWKS cache) | RP **and** OP; also resource-server helpers (`rs` introspection) | OAuth2 transport/flows only, no OIDC |
| Discovery | `oidc.NewProvider(ctx, issuer)` (+`InsecureIssuerURLContext` for mismatched issuers, `ProviderConfig{...}.NewProvider` for manual endpoints); `Endpoint()` returns `{AuthURL, TokenURL, DeviceAuthURL}` | `rp.NewRelyingPartyOIDC(ctx, issuer, clientID, secret, redirect, scopes, opts...)`, `WithCustomDiscoveryUrl`, full `DiscoveryConfiguration` incl. `end_session_endpoint`, `revocation_endpoint` | n/a |
| Auth code + PKCE | via x/oauth2 (`oauth2.GenerateVerifier`, `S256ChallengeOption`, `VerifierOption`) | `WithPKCE(cookieHandler)` / `WithPKCEFromDiscovery`; stores verifier in an encrypted cookie | yes (since v0.13.0) |
| Device flow | no helpers, but `Endpoint().DeviceAuthURL` is populated from discovery | `rp.DeviceAuthorization`, `rp.DeviceAccessToken` (handles `slow_down`) | `Config.DeviceAuth(ctx)` -> `DeviceAuthResponse{DeviceCode, UserCode, VerificationURI, VerificationURIComplete, Expiry, Interval}`; `Config.DeviceAccessToken(ctx, da)` polls |
| ID-token verify | `provider.Verifier(&oidc.Config{ClientID, SupportedSigningAlgs, Skip* flags}).Verify(ctx, raw)`; checks iss/aud/exp/sig; `IDToken.Claims(&v)`, `VerifyAccessToken(at_hash)`, `Nonce()` option; v3.15 verifies signature before parsing claims | `rp.VerifyTokens`, generic `oidc.Tokens[C]` with typed claims | n/a |
| Refresh | via `oauth2.Config.TokenSource(ctx, tok)` | `rp.RefreshTokens` | yes |
| Logout | nothing built in; read `end_session_endpoint` through `provider.Claims(&struct{...})` | `GetEndSessionEndpoint()`, `rp.EndSession` (**UNVERIFIED** name), `GetRevokeEndpoint()` | n/a |
| Token exchange (RFC 8693) / JWT profile (RFC 7523) | no | yes | no |
| Back-channel logout | no | OP only ("not yet" for RP) | no |
| CLI helper | no | `rp/cli.CodeFlow` + `OpenBrowser` | no |
| Deps / size | tiny: go-jose, x/oauth2 | larger: go-jose, otel (opt-out `no_otel` build tag), global `slog` | stdlib |
| Maintenance | steady (v3.17 Nov 2025 ... v3.21 Sep 2026); used by >1000 modules | active, but maintainers state "limited availability for feature reviews"; v4 is in `-next` pre-releases | Go team |
| Certification | none claimed | RP certified for Basic & Config profiles | n/a |
| License | Apache-2.0 | Apache-2.0 | BSD-3-Clause |

Recommendation: **go-oidc + x/oauth2**. Everything kudurru needs for a BFF (discovery, verifier with JWKS caching,
PKCE, device flow, refresh) exists there with a small dependency surface, and go-oidc is also the right tool for
verifying Kubernetes SA tokens (it is listed as a supported "workload" issuer). Write ~150 lines of glue for: reading
`end_session_endpoint`, RP-initiated logout URL building, and a device-flow CLI loop (copy zitadel's `slow_down`
handling). Revisit zitadel/oidc if kudurru ever needs to *be* an OP (e.g. issuing tokens to integrations) — that is a
separate decision.

## 4. Flows

### 4.1 Web UI: authorization code + PKCE, server-side session (BFF)

Why BFF: RFC 10017 (BCP 212) ranks "Backend For Frontend" above "Token-Mediating Backend" above browser-only
clients; tokens never reach JavaScript; the SPA only holds an `HttpOnly` cookie, so XSS cannot exfiltrate long-lived
credentials (contrast Technitium's `localStorage` token). RFC 9700 (BCP 240) requires PKCE for code flows, exact
redirect-URI matching, `state`/`nonce`, and forbids the implicit grant.

Sequence:
1. `GET /auth/login?provider=<id>&next=/zones` -> server creates `{state, nonce, code_verifier, next, provider}`,
   stores it in a short-lived (10 min) **pre-auth cookie** (encrypted, `SameSite=Lax`) or the DB; redirects to
   `AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier), oauth2.AccessTypeOffline?)`.
2. `GET /auth/callback?code&state` -> validate state, `Exchange(ctx, code, oauth2.VerifierOption(verifier))`,
   `Verifier.Verify(id_token)`, check `nonce`, optionally call userinfo (Headscale merges it when `sub` matches),
   run claim validation + mapping (section 4.3), upsert identity/user, **create server-side session**, rotate the
   session ID, set cookie, redirect to `next` (same-origin paths only).
3. Session cookie: `kudurru_session=<random 32 bytes base64url>; Path=/; HttpOnly; Secure; SameSite=Lax`;
   absolute lifetime (default 12 h, max 30 d) + idle timeout (default 30 min, UI keeps it alive like Pi-hole);
   store = `sessions` table (SQLite/Postgres) — in k8s all replicas share the DB so any pod can serve any session
   (Gravity's etcd store is the same idea). `alexedwards/scs` provides exactly this (`Lifetime`, `IdleTimeout`,
   `Cookie.{Secure,HttpOnly,SameSite,Persist}`, `LoadAndSave`, stores for sqlite3/postgres/etcd/redis/bbolt) and is
   a reasonable base; otherwise ~200 lines hand-rolled.
4. Session record keeps the IdP `refresh_token` (encrypted at rest with the server's data-encryption key) and the
   access-token expiry so the server can (a) refresh in the background when a UI needs upstream tokens (rare for
   kudurru), and (b) re-evaluate group membership by refreshing the ID token every `reauth_interval` (default 1 h,
   Grafana's `use_refresh_token` behaviour). If refresh fails -> session invalidated.
5. Logout: `POST /auth/logout` deletes the session row + cookie; if the provider exposes `end_session_endpoint`,
   redirect to it with `id_token_hint` and `post_logout_redirect_uri` (OIDC RP-Initiated Logout 1.0). Implement the
   OIDC **Back-Channel Logout** receiver (`POST /auth/backchannel-logout` with a `logout_token` JWT, events claim
   `http://schemas.openid.net/event/backchannel-logout`; kill sessions by `sid`/`sub`) as a v2 item.
6. Multiple providers: `identity_providers` is a list, each with an `id` (slug), `displayName`, `issuer`,
   `clientId`, `clientSecretRef`, `scopes`, `discoveryURL?`, `caBundle?`, mapping rules, `autoProvision` policy.
   The login page lists buttons; `state` carries the provider id. Identity key is `(provider_id, iss, sub)` — never
   email (Headscale). Issuer URLs unique across providers (k8s rule) so bearer JWTs can be routed by `iss`.

CSRF for cookie sessions:
- `http.NewCrossOriginProtection()` (Go 1.25) wrapping all mutating routes: rejects non-safe methods whose
  `Sec-Fetch-Site` is cross-site, or whose `Origin` host differs from `Host`; `AddTrustedOrigin("https://ui.example")`
  for split deployments; `AddInsecureBypassPattern` only for webhook endpoints that use bearer auth. Caveats: requires
  HTTPS for browsers to send `Sec-Fetch-Site`; `Origin`-vs-`Host` fallback cannot distinguish http/https of the same
  host, so enable HSTS.
- Defence in depth (Pi-hole style): the login/session-info response returns a per-session CSRF token; cookie-authenticated
  `/api/*` writes must echo it in `X-Kudurru-CSRF`. Bearer-authenticated requests are exempt (browsers cannot add
  custom headers cross-site without CORS pre-flight).
- `SameSite=Lax` keeps the OIDC callback (top-level GET navigation) working while blocking cross-site POSTs.
- Security headers like dnsdist's defaults: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, CSP
  `default-src 'self'`.

### 4.2 CLI (`kudurru login`)

Two modes, auto-selected:
1. **Loopback + PKCE (default when a browser can be opened)**: bind `127.0.0.1:0` (or a configured port list like
   kubelogin's `8000,18000` so the IdP redirect URI can be pre-registered as `http://localhost:8000/callback` and
   `http://127.0.0.1:8000/callback`), open the browser, receive the code, exchange with PKCE, print "you may close
   this window". Public client (no secret) registered at the IdP as "kudurru-cli" with loopback redirects; RFC 8252
   permits any loopback port — most IdPs (Keycloak, Authentik, Entra, Okta) honour this.
2. **Device flow** (`--device`, or when no browser/`SSH_TTY`/`KUDURRU_NO_BROWSER`): `oauth2.Config.DeviceAuth` with
   `oidc.ScopeOpenID` etc., print `user_code` + `verification_uri` (+ QR?), poll `DeviceAccessToken` honouring
   `interval`/`slow_down`; show the client name and warn per RFC 10027.
3. Alternative when the IdP has neither (rare): `kudurru login --token` to paste a kudurru API token; and
   `--username/--password` against the local account store (needed for the bootstrap admin, disabled for SSO users).

Token handling: cache `{id_token, refresh_token, expiry, provider}` in the OS keyring (`zalando/go-keyring`) with a
file fallback (`$XDG_CONFIG_HOME/kudurru/credentials.json`, mode 0600); request `offline_access` only when the IdP
needs it for refresh tokens. The CLI sends `Authorization: Bearer <id_token>`; the server:
- routes by `iss` to the matching identity provider, verifies signature/exp/aud (`aud` must contain the CLI client
  id or the server client id — configure `audiences: [kudurru-web, kudurru-cli]` like k8s `audienceMatchPolicy:
  MatchAny`),
- caches the verification result keyed by `sha256(token)` until `exp` (bounded LRU) to avoid re-parsing per call,
- re-runs claim mapping each time (group changes propagate at the token lifetime, typically 5-60 min).
No server-side state is needed for CLI sessions; revocation = disable the identity at the IdP or in kudurru
(`users.disabled` is checked on every request). This is Gravity's `tokenUsernameField` JWT mode done properly.

### 4.3 Claims -> identity, groups, roles

Three layers, applied in this order (all configurable per identity provider):
1. **Validation** (`claimValidationRules`): list of `{claim, requiredValue}` or `{expression, message}` (CEL). Built-in
   shortcuts mirroring Headscale: `allowedDomains`, `allowedUsers` (emails; only if `email_verified` or
   `emailVerifiedRequired: false`), `allowedGroups`.
2. **Mapping** (`claimMappings`): `username {claim|expression, prefix}` (default `preferred_username`, fallback order
   `preferred_username -> upn -> email -> sub`), `displayName`, `email`, `groups {claim|expression, prefix}` (default
   `groups`, accept string-or-array like Headscale's `FlexibleStringSlice`, also read `roles` by default like
   Technitium), `uid` (default `sub`). CEL env: `claims` (map) + `userinfo` (map); result types checked at config load.
3. **Authorization binding**: `groupMap: [{remote: "/dns-admins", local: "dns-admins"}]` (Technitium) **or** direct
   `roleBindings` on the *remote group string* (`subject: {kind: Group, name: "oidc:/dns-admins"}`) so no local group
   object is required (Kubernetes style, `--oidc-groups-prefix`). A `strict` flag (Grafana `role_attribute_strict`)
   refuses login when the user ends with zero bindings; the default assigns `viewer` only if `defaultRole` is set.
   Provisioning policy: `autoProvision: always | mapped-only | never` (Technitium's three states) and `syncGroups:
   true` (groups become IdP-managed; local edits locked, membership *replaced* at each login).

Engine choice: **cel-go** (Apache-2.0; same semantics as the Kubernetes config, so operators can reuse knowledge and
examples; sandboxed, non-Turing-complete, cost-limited). `expr-lang/expr` (MIT) is lighter but a different dialect;
JMESPath (Grafana) is weakest for boolean logic. Compile expressions at config-load time and fail fast with
`message`.

### 4.4 Local admin fallback (bootstrap)

- On first start with an empty user table: create `admin` with password from `KUDURRU_ADMIN_PASSWORD` or
  `KUDURRU_ADMIN_PASSWORD_FILE` (Docker/K8s secret mounts; Technitium and Pi-hole do `_FILE`/`FTLCONF_` env), else
  generate 32 random bytes, print once to stdout like Gravity, and set `must_change_password=true`. Expose
  `hasDefaultCredentials` in `/api/v1/auth/status` so the UI nags (Technitium).
- Optional `KUDURRU_BOOTSTRAP_TOKEN` creates a service-account token at first start (Gravity `ADMIN_TOKEN`) so
  provisioning (Terraform/Helm post-install Job) never needs the password; in k8s the chart can generate it into a
  Secret and the operator reads it.
- Local login remains available at `/login?local=1` even when SSO auto-redirect is on (Gravity `?local`), and can be
  disabled per-policy (`localLogin: disabled|admins-only|enabled`). Authelia's Technitium guide explicitly says
  "keep one local administrator account for break-glass".
- Local recovery without HTTP: `kudurru admin reset-password` and `kudurru admin create-token` over the **unix
  socket** (`/run/kudurru/kudurru.sock`, mode 0660; identity = "local-root") exactly like Headscale's CLI socket and
  Pi-hole's `cli_pw` — in Docker `docker exec kudurru kudurru admin reset-password`, in k8s `kubectl exec`.
- 2FA (TOTP) for local accounts as in Technitium/Pi-hole/PowerDNS-Admin: rate-limit to one attempt per second and
  remember the last accepted time-step (Pi-hole) to prevent replay. SSO users never get a local password.

### 4.5 API tokens and service accounts

Model (Grafana + Headscale + PowerDNS-Admin merged):
- `service_accounts`: non-human principals (`name`, `description`, `disabled`, `created_by`), owning role bindings
  like users. Examples: `external-dns`, `unifi-sync`, `cloudflare-sync`, `prometheus`.
- `api_tokens`: `{id, kind: service|personal, owner (sa_id|user_id), name, token_id (12 chars, indexed),
  secret_hash (sha256 hex), created_at, expires_at?, last_used_at, last_used_ip, revoked_at, scopes?}`. Wire format
  `kdr_<token_id>_<secret 32 bytes base64url>` so secret scanners can fingerprint it (`kdr_` prefix). Shown once.
  Verification: split on `_`, look up `token_id`, constant-time compare `sha256(secret)` — never prefix-search
  (Technitium v15.6 oracle). Unknown id and wrong secret produce the same 401.
- Personal tokens inherit the owner's effective bindings *intersected* with optional `scopes` (e.g. `zones:read`,
  `zone:example.com:write`); when the user is disabled every personal token dies (Grafana's argument for service
  accounts: they survive user departure, personal tokens should not).
- Expiry policy `auth.tokens.maxLifetime` (Grafana `token_expiration_day_limit`); `last_used_*` for hygiene reports;
  audit every create/revoke.
- Integration secrets that kudurru *holds* (dnsdist `api_key`, Cloudflare API token, UniFi credentials) are a
  different table (`integration_credentials`, encrypted with the data-encryption key, or a k8s `secretRef`) — they
  are outbound credentials, not principals.

### 4.6 Kubernetes-hosted deployments: projected ServiceAccount tokens as identity

Two verification paths, both producing principal `system:serviceaccount:<ns>:<sa>` with groups
`system:serviceaccounts`, `system:serviceaccounts:<ns>`:
1. **JWKS path (preferred, no API call per request)**: at startup inside a cluster, register an implicit identity
   provider `kubernetes` with `issuer = <cluster --service-account-issuer>` (read from
   `https://kubernetes.default.svc/.well-known/openid-configuration` using the pod's own token; needs the default
   `system:service-account-issuer-discovery` binding), `audiences: ["kudurru"]`, `claimMappings.username.claim: sub`,
   `groups.expression: ["system:serviceaccounts", "system:serviceaccounts:" + claims["kubernetes.io"].namespace]`.
   Verification is then exactly the OIDC bearer path with go-oidc (`InsecureIssuerURLContext` is sometimes needed
   because the discovery doc's `issuer` may differ from the in-cluster URL; otherwise set `discoveryURL`).
2. **TokenReview fallback**: `clientset.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{Spec:
   {Token: t, Audiences: []string{"kudurru"}}})`; require `status.authenticated` **and** `"kudurru" ∈
   status.audiences`; cache positive results for ≤ 2 min keyed by token hash (the API server's own webhook cache
   default). Needs ClusterRole rule `apiGroups: [authentication.k8s.io], resources: [tokenreviews], verbs: [create]`.
Callers (e.g. an in-cluster controller or CI job) mount a projected token with `audience: kudurru` and send it as
`Authorization: Bearer`. Bindings target `subject: {kind: ServiceAccount, namespace: ..., name: ...}`.
Also accept the CRD route: a `KudurruServiceAccount` object can declare `kubernetesSubject` so no token exchange is
ever needed. If the pod lacks RBAC for discovery/TokenReview, log once and disable the provider — never fail open.

## 5. RBAC engine comparison

| Option | Shape | Pros | Cons | Verdict |
|---|---|---|---|---|
| Hand-rolled roles + bindings | `permission = (resource, verb)`; `role = set of permissions`; `binding = (subject, role, scope)`; evaluation = lookup | Zero deps, trivially explainable in UI ("why can I see this?"), easy to render permission matrix, scope semantics (`zone:<name>`, `cluster`) are first-class, maps 1:1 to CRDs and to k8s RBAC mental model | Must write tests for inheritance (group -> user, scope -> global) and the matrix must be kept in sync with handlers (use a code-generated table + a lint that every route declares a permission) | **Recommended** |
| Casbin v3.11.0 (Apache-2.0, in-process) | PERM model file (`rbac_with_domains`: `r = sub, dom, obj, act`; `g = _, _, _`), policy rows in CSV/DB adapters, watchers (etcd/redis) for multi-replica sync, `SyncedCachedEnforcer` | Battle-tested matchers, ABAC mixins, deny-override, domains = zone scope out of the box, many adapters | Policy stored as opaque tuples, harder to surface in UI; reflective matcher evaluation; two sources of truth if bindings also live in your tables; model DSL is a learning curve for contributors; go 1.13 compatibility baggage | Viable fallback; can be introduced later behind the same `Authorizer` interface |
| OpenFGA (Apache-2.0, CNCF sandbox, Okta) / SpiceDB (Apache-2.0, AuthZed) | Zanzibar relationship graph, separate service (Postgres/MySQL; SpiceDB also CRDB/Spanner), gRPC/HTTP check API | Rich ReBAC (zone -> records inherit, groups of groups), consistency tokens | A second stateful service to deploy in Docker single-instance mode, network hop per check, overkill for ~12 resource types and ≤ thousands of zones | Out of scope |

## 6. Resource model, roles and permission matrix for a DNS server

Resources (verbs are `read`, `write`, `delete`; some have extra verbs):

| Resource | Scope-able per zone? | Extra verbs | Notes |
|---|---|---|---|
| `zones` | yes (`zone:<name>` and `zonegroup:<tag>`) | `transfer` (AXFR/notify config), `sign` (DNSSEC ops), `share` (edit zone ACL) | creating a zone requires cluster-scope `zones.write`; creator receives `zone-admin` on it (Technitium/Authentik "initial permissions") |
| `records` | yes (inherits zone scope) | — | record-type restrictions (e.g. no NS/SOA/DNSKEY for `operator`) are a policy on top |
| `blocklists` | no (global) + `clientgroup:<name>` | `refresh` | lists, allow/deny rules, adblock syntax |
| `policies` | no | — | response policy / RPZ-like rules, client-group policies |
| `clients` & `clientgroups` | `clientgroup:<name>` | — | Pi-hole/AdGuard style client groups |
| `querylog` | `clientgroup:<name>` (so a group owner sees only their clients) | `export`, `purge` | **privacy**: separate `auditor` role; admins can disable client IP display; retention config is `settings` |
| `stats` | no | — | aggregated dashboard counters (non-PII) readable by `viewer` |
| `cache` | no | `flush` | |
| `settings` | no | — | server config, DoH/DoT/DoQ listeners, forwarders, dnsdist/keepalived integration config |
| `users`, `groups`, `serviceaccounts`, `tokens`, `identityproviders`, `rolebindings` | no | `impersonate`? (no) | IAM; `tokens.write` on *own* principal is always allowed for personal tokens |
| `integrations` | no | `sync`, `test` | Cloudflare/UniFi connectors and their credentials |
| `cluster` | no | `join`, `drain` | nodes, leader, VIP (keepalived) state |
| `audit` | no | `export` | audit log read |

Built-in roles (bindable at `cluster` scope, and `zone-admin`/`operator`/`viewer` also at `zone:*`/`zonegroup:*`
scope):

| Permission | viewer | auditor | operator | zone-admin | admin |
|---|---|---|---|---|---|
| zones.read / records.read / stats.read / cache.read | ✓ | ✓ | ✓ | ✓ (scoped) | ✓ |
| records.write / records.delete | | | ✓ (scoped or global) | ✓ (scoped) | ✓ |
| zones.write (create/edit options) / zones.delete | | | | ✓ scoped edit; create needs cluster scope | ✓ |
| zones.transfer / zones.sign (DNSSEC) | | | | ✓ (scoped) | ✓ |
| zones.share (manage zone bindings) | | | | ✓ (scoped) | ✓ |
| blocklists.read / policies.read / clients.read | ✓ | ✓ | ✓ | | ✓ |
| blocklists.write / policies.write / clients.write / cache.flush | | | ✓ | | ✓ |
| querylog.read (with client IPs) | | ✓ | ✓ (configurable: `operatorSeesQueryLog`) | | ✓ |
| querylog.export / querylog.purge | | ✓ export | | | ✓ |
| settings.read | ✓ (redacted) | ✓ | ✓ | | ✓ |
| settings.write / integrations.* / cluster.* | | | | | ✓ |
| users/groups/serviceaccounts/rolebindings/identityproviders.* | | | | | ✓ |
| tokens.write on own principal | ✓ | ✓ | ✓ | ✓ | ✓ |
| audit.read | | ✓ | | | ✓ |

Evaluation algorithm (pure function, unit-testable):
```
effective(subject) = bindings where binding.subject ∈ {user, user's groups (local + IdP-mapped),
                     "system:authenticated", service account}
allow(p, resource, verb, scope) =
   ∃ b ∈ effective(p): role(b).grants(resource, verb)
      ∧ (b.scope == cluster ∨ b.scope == scope ∨ scope ∈ expand(b.scope))   // zonegroup -> zones
   ∧ ¬ principal.disabled ∧ token.scopes ⊇ {resource:verb} (if token-scoped)
```
Zone listing = filter zones by `allow(p, zones, read, zone:<name>)`; precompute a per-request scope set for speed
(Technitium iterates permissions per zone, which is O(zones × groups) but fine up to thousands).

## 7. Security details

**Password hashing**: Argon2id via `golang.org/x/crypto/argon2` with OWASP minimum `m=19456 KiB, t=2, p=1`
(alternatives m=46 MiB/t=1 or m=12 MiB/t=3); store PHC strings `$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>` so
parameters can be upgraded at next login (OWASP). Cap password length (e.g. 1 KiB) and enforce a global
Argon2 concurrency semaphore (Headscale's `legacyHashLimiter`) because hashing is reachable unauthenticated.
Comparison: Technitium PBKDF2-SHA256 100k (OWASP says 600k for SHA-256), Gravity/AdGuard/PDA bcrypt cost 10,
Pi-hole Balloon-SHA256 s=1024,t=32.

**Login rate limiting** (both local login and token endpoints): per-IP bucket (respect `trustedProxies` like AdGuard,
else `RemoteAddr`) with 5 failures -> 5 min block (Technitium) *and* per-username bucket (10 failures -> 15 min) to
blunt distributed guessing; constant 500 ms-1 s delay on failure; `429` with `Retry-After` (AdGuard); dummy Argon2
verification for unknown users to equalise timing (Gravity does this with bcrypt); TOTP one attempt/second with
counter replay protection (Pi-hole). Bearer-token failures count toward the per-IP bucket too.

**Session hygiene**: random 256-bit IDs; store `sha256(session_id)` in the DB (scs `HashTokenInStore`) so a DB leak
does not yield live sessions; record `ip`, `user_agent`, `created_at`, `last_seen`, `tls_at_login`; rotate on login
and privilege change; invalidate all sessions on password change / app-password creation (Pi-hole); admin UI to list
and kill sessions (Technitium/Pi-hole), listing by session id hash prefix is fine for *display* but deletion must use
the full internal id. `max_sessions` per user (Pi-hole global 16) optional.

**Audit log** ("who changed which record"): append-only `audit_events` table + structured log line + optional
webhook. Fields: `id (uuid v7)`, `ts`, `actor {principal_type, principal_id, display, token_id?, session_id?,
impersonated_by?}`, `source {ip, user_agent, request_id, via: ui|api|cli|k8s|cluster}`, `action` (`records.update`),
`resource {kind, name, zone, id}`, `outcome {ok|denied|error}`, `before`/`after` (JSON, redact secrets), `reason`
(optional free text the UI can require for prod zones). Write from the authz/handler layer (not from the DB layer)
so denied attempts are also recorded. Retention + export (`audit.export`). PowerDNS-Admin's `History` and
Grafana-style per-object history pages are the UX reference. Login/logout/token-create/revoke/binding changes are
audit events too.

**Secret storage for OIDC client secret / integration credentials**:
- Config supports `clientSecret` (discouraged), `clientSecretFile` (Docker secrets `/run/secrets/...`, systemd
  `${CREDENTIALS_DIRECTORY}` like Headscale) and env `KUDURRU_AUTH_<PROVIDER>_CLIENT_SECRET[_FILE]` (Technitium pattern).
- In k8s the `IdentityProvider` CRD carries `clientSecretRef: {name, key}` to a `Secret`; the operator never copies the
  value into the CRD status. Prefer `client_secret_post`/`client_secret_basic` with a confidential client; support
  `private_key_jwt` (RFC 7523) later for secret-less registration.
- Secrets held in the DB (refresh tokens, integration credentials) are encrypted with a server **data-encryption key**
  (`KUDURRU_ENCRYPTION_KEY[_FILE]`, 32 bytes, AES-256-GCM, key id in the ciphertext header for rotation). In k8s the
  key is a Secret the chart generates once. API responses mask secrets (`"************"`, Technitium).
- Never log tokens; log masked prefixes (`kdr_abc123…`, Headscale `maskedPrefix`).

**Transport**: the API/UI must be served over TLS (or behind an ingress that is); `Secure` cookie attribute is
mandatory except for `localhost` dev (AdGuard omits it — avoid). HSTS when TLS terminates in kudurru.

## 8. Concrete design for kudurru

### 8.1 Identities and principals

```
Principal   = User | ServiceAccount | KubernetesServiceAccount | Node (cluster peer) | LocalRoot (unix socket)
User        = { id, username (unique, lowercase), display_name, email, disabled, must_change_password,
                password_hash? (nil for SSO-only), totp_secret?, created_at, last_login_at, last_login_ip }
Identity    = { id, user_id, provider_id ("local" | idp slug | "kubernetes"), issuer, subject, email,
                groups_snapshot[], last_seen_at }     // a user can have several identities (local + OIDC)
Group       = { id, name, description, managed_by ("local" | provider_id) }  + group_members(user_id, group_id)
ServiceAccount = { id, name, description, disabled, kubernetes_subject? (ns/name) }
```
Subjects usable in bindings: `User:<username>`, `Group:<name>` (local or `oidc:<provider>:<remote-group>` without
creating a local group), `ServiceAccount:<name>`, `K8sServiceAccount:<ns>/<name>`, `system:authenticated`.

### 8.2 Token types

| Token | Issued by | Transport | Lifetime | Stored server-side | Revocable | Used by |
|---|---|---|---|---|---|---|
| Browser session | kudurru after local login or OIDC callback | `kudurru_session` cookie (HttpOnly, Secure, SameSite=Lax) + `X-Kudurru-CSRF` header for writes | idle 30 min / abs 12 h (configurable) | yes (`sessions`) | yes (self, admin, password change) | SPA |
| OIDC ID token (bearer) | IdP (web client id or CLI public client id) | `Authorization: Bearer` | IdP-defined (5-60 min), refreshed by the CLI with its refresh token | no (verification cache only) | at IdP, or disable user/identity in kudurru | CLI, automation with workload identity (GitHub Actions OIDC, Vault, k8s SA) |
| Kubernetes SA token | kube-apiserver (projected, `aud: kudurru`) | `Authorization: Bearer` | ≤ expirationSeconds (1 h typ.) | no (cache ≤ 2 min for TokenReview path) | at k8s | in-cluster controllers/jobs |
| API token `kdr_…` | kudurru (service account or personal) | `Authorization: Bearer` | none or `expires_at` ≤ policy max | hash only (`api_tokens`) | yes | integrations, scripts, Terraform, dnsdist/keepalived sidecars |
| Cluster peer credential | kudurru (per node, mTLS cert or shared join token) | mTLS / bearer on the internal port | node lifetime | yes (`nodes`) | yes (`cluster.drain`) | replication, config sync |
| Bootstrap token | env `KUDURRU_BOOTSTRAP_TOKEN` at first start | same as API token | until revoked | hash | yes | initial provisioning |
| Local root | unix socket peer credentials (SO_PEERCRED uid 0 / configured gid) | socket | per connection | no | n/a | `kudurru admin …` recovery |

### 8.3 Middleware layout (Go, `net/http` + `chi`/stdlib mux)

```
Recover -> RequestID -> RealIP(trustedProxies) -> SecurityHeaders -> AccessLog
  -> CrossOriginProtection (Go 1.25) [mutating routes]
  -> RateLimit(login/token endpoints; per-IP + per-user)
  -> Authenticate (first match wins, else anonymous):
        1. Authorization: Bearer kdr_…           -> api_tokens lookup (sha256) -> ServiceAccount|User principal
        2. Authorization: Bearer <JWT>           -> parse iss (unverified) -> provider by issuer
                                                     -> go-oidc Verify (JWKS cache) -> claim mapping -> principal
                                                     ; issuer == cluster SA issuer -> k8s mapping (or TokenReview)
        3. Cookie kudurru_session                -> sessions table -> User principal; require X-Kudurru-CSRF on writes
        4. Unix socket peer                      -> LocalRoot
  -> LoadPrincipal (disabled? must_change_password? -> 403 with reason)
  -> Authorize(resource, verb, scope) via Authorizer interface {Allow(ctx, p, Request) Decision}
        (route table declares {resource, verb, scopeFromPath}); 401 if anonymous, 403 otherwise
  -> Handler
  -> Audit (deferred; records outcome, before/after from handler context)
```
Interfaces to keep swappable: `Authenticator` (chain), `Authorizer` (hand-rolled now; Casbin adapter possible),
`SessionStore`, `TokenStore`, `IdentityProviderRegistry` (hot-reload on CRD/config change; retry discovery with
backoff like Gravity and optionally `onlyStartIfAvailable` like Headscale).

### 8.4 Data model — DB tables (SQLite in single-instance, Postgres in clustered/k8s)

```sql
CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT UNIQUE NOT NULL, display_name TEXT, email TEXT,
  password_hash TEXT, totp_secret_enc BLOB, disabled BOOL NOT NULL DEFAULT 0, must_change_password BOOL NOT NULL DEFAULT 0,
  created_at, updated_at, last_login_at, last_login_ip);
CREATE TABLE identities (id TEXT PRIMARY KEY, user_id TEXT REFERENCES users ON DELETE CASCADE, provider_id TEXT NOT NULL,
  issuer TEXT NOT NULL, subject TEXT NOT NULL, email TEXT, groups_json TEXT, last_seen_at, UNIQUE(provider_id, issuer, subject));
CREATE TABLE groups (id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, description TEXT, managed_by TEXT NOT NULL DEFAULT 'local');
CREATE TABLE group_members (group_id TEXT, user_id TEXT, PRIMARY KEY(group_id, user_id));
CREATE TABLE service_accounts (id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, description TEXT, disabled BOOL,
  k8s_namespace TEXT, k8s_name TEXT, created_by TEXT, created_at);
CREATE TABLE api_tokens (id TEXT PRIMARY KEY, token_id TEXT UNIQUE NOT NULL, secret_hash TEXT NOT NULL,
  kind TEXT NOT NULL CHECK(kind IN ('service','personal')), owner_type TEXT NOT NULL, owner_id TEXT NOT NULL, name TEXT,
  scopes_json TEXT, created_at, expires_at, last_used_at, last_used_ip, revoked_at, created_by);
CREATE TABLE sessions (id_hash TEXT PRIMARY KEY, user_id TEXT NOT NULL, identity_id TEXT, csrf_token TEXT NOT NULL,
  created_at, last_seen_at, expires_at, ip TEXT, user_agent TEXT, idp_refresh_token_enc BLOB, idp_token_expires_at,
  idp_sid TEXT);
CREATE TABLE roles (name TEXT PRIMARY KEY, builtin BOOL, description TEXT, permissions_json TEXT);  -- builtins seeded
CREATE TABLE role_bindings (id TEXT PRIMARY KEY, subject_kind TEXT NOT NULL, subject_name TEXT NOT NULL,
  role TEXT NOT NULL REFERENCES roles, scope_kind TEXT NOT NULL CHECK(scope_kind IN ('cluster','zone','zonegroup','clientgroup')),
  scope_name TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT 'api',  -- 'api' | 'crd:<ns>/<name>' | 'bootstrap'
  created_at, created_by, UNIQUE(subject_kind, subject_name, role, scope_kind, scope_name));
CREATE TABLE identity_providers (id TEXT PRIMARY KEY, display_name TEXT, issuer TEXT UNIQUE, discovery_url TEXT,
  client_id TEXT, client_secret_enc BLOB, scopes_json TEXT, ca_bundle TEXT, mapping_json TEXT, provisioning_json TEXT,
  enabled BOOL, source TEXT);
CREATE TABLE login_attempts (key TEXT PRIMARY KEY, failures INT, blocked_until);        -- or in-memory + gossip
CREATE TABLE audit_events (id TEXT PRIMARY KEY, ts, actor_json TEXT, source_json TEXT, action TEXT, resource_json TEXT,
  outcome TEXT, before_json TEXT, after_json TEXT, reason TEXT);
CREATE INDEX audit_ts ON audit_events(ts); CREATE INDEX audit_resource ON audit_events(action, ts);
```

### 8.5 Kubernetes CRDs (group `kudurru.io/v1alpha1`) — declarative subset only

```yaml
apiVersion: kudurru.io/v1alpha1
kind: IdentityProvider            # cluster-scoped
metadata: { name: corp-sso }
spec:
  displayName: "Corp SSO"
  issuer: https://auth.example.com/application/o/kudurru/
  clientID: kudurru
  clientSecretRef: { name: kudurru-oidc, key: clientSecret, namespace: kudurru }
  cliClientID: kudurru-cli         # public client for device/loopback flows (optional, defaults to clientID)
  scopes: [openid, profile, email, groups]
  audiences: [kudurru, kudurru-cli]
  claimValidationRules:
  - expression: 'claims.email_verified == true'
    message: email must be verified
  claimMappings:
    username: { claim: preferred_username }
    groups:   { claim: groups, prefix: "corp:" }
  provisioning: { autoProvision: mapped-only, syncGroups: true, defaultRole: "" }
  allowedDomains: [example.com]
status: { discovered: true, lastDiscovery: ..., conditions: [...] }
---
apiVersion: kudurru.io/v1alpha1
kind: RoleBinding                 # namespaced (same namespace as the Zone CRs it refers to)
metadata: { name: platform-team-zone-admins, namespace: dns }
spec:
  role: zone-admin                # builtin or a KudurruRole name
  scope: { kind: zone, name: example.com }      # or {kind: cluster}, {kind: zonegroup, name: prod}
  subjects:
  - { kind: Group, name: "corp:/dns-admins" }
  - { kind: ServiceAccount, name: external-dns, namespace: kube-system }   # k8s SA via projected token
  - { kind: User, name: alice }
---
apiVersion: kudurru.io/v1alpha1
kind: ServiceAccount              # kudurru-internal principal that needs an API token
metadata: { name: unifi-sync, namespace: dns }
spec:
  description: "UniFi controller integration"
  token:
    secretName: unifi-sync-token  # operator generates kdr_… once, writes it here, stores only the hash
    expiresAfter: 8760h
status: { tokenID: "abc123def456", createdAt: ..., lastUsedAt: ... }
```
`Role` (custom permission sets) is a fourth, optional CRD. The operator reconciles these into the tables with
`source = crd:<ns>/<name>`; API/UI edits to CRD-sourced rows are rejected ("managed by Kubernetes"), exactly like
Technitium locks IdP-managed groups. Runtime objects (sessions, audit, login attempts) are never CRDs.
Leader election only matters for the reconciler; every replica authenticates independently because the DB is shared.

### 8.6 Config file (Docker single instance) — same fields, YAML

```yaml
auth:
  localLogin: enabled                 # enabled | admins-only | disabled
  bootstrap: { adminPasswordFile: /run/secrets/admin_password, tokenFile: /run/secrets/bootstrap_token }
  session: { idleTimeout: 30m, lifetime: 12h, cookieName: kudurru_session, secure: true, sameSite: lax }
  password: { argon2: { memoryKiB: 19456, time: 2, parallelism: 1 } }
  rateLimit: { perIP: { failures: 5, block: 5m }, perUser: { failures: 10, block: 15m }, trustedProxies: [10.0.0.0/8] }
  tokens: { maxLifetime: 8760h, prefix: kdr_ }
  identityProviders:
  - id: corp-sso
    issuer: https://auth.example.com/application/o/kudurru/
    clientID: kudurru
    clientSecretFile: /run/secrets/oidc_client_secret   # or clientSecret / env KUDURRU_AUTH_CORP_SSO_CLIENT_SECRET
    scopes: [openid, profile, email, groups]
    claimMappings: { username: { claim: preferred_username }, groups: { claim: groups, prefix: "corp:" } }
    provisioning: { autoProvision: mapped-only, syncGroups: true }
  kubernetes: { enabled: auto, audience: kudurru, mode: jwks }   # auto = only when in-cluster
rbac:
  bindings:
  - { role: admin, scope: cluster, subjects: [ { kind: Group, name: "corp:/dns-admins" } ] }
  - { role: zone-admin, scope: { kind: zone, name: example.com }, subjects: [ { kind: User, name: alice } ] }
```
Bindings from the file load with `source: config` and behave like CRD-sourced rows. `docker run -e
KUDURRU_ADMIN_PASSWORD=… -p 53:53/udp -p 8080:8080 kudurru` works with zero auth config (local admin only).

### 8.7 HTTP API surface (auth-related, `/api/v1`)

```
GET  /auth/status                      -> { localLogin, providers:[{id,displayName}], hasDefaultCredentials, authenticated, principal?, csrf? }
POST /auth/login                       {username,password,totp?}            -> session cookie (+ 2fa-required)
GET  /auth/oidc/{provider}/start?next= -> 302 to IdP;  GET /auth/oidc/{provider}/callback
POST /auth/logout ; POST /auth/backchannel-logout (OIDC back-channel, v2)
GET  /auth/me                          -> principal, identities, effective bindings, permissions matrix for UI gating
GET/DELETE /auth/sessions[/{id}]       (own); GET/DELETE /admin/sessions (admin)
POST /auth/tokens  {name, expiresAt?, scopes?} -> {token: "kdr_…"} once ; GET/DELETE /auth/tokens/{id}
CRUD /admin/users, /admin/groups, /admin/service-accounts, /admin/service-accounts/{id}/tokens,
     /admin/identity-providers (secret write-only), /admin/role-bindings, /admin/roles
GET  /zones/{zone}/bindings ; PUT …    (requires zones.share on that zone)
GET  /audit?actor=&resource=&since=    ; GET /audit/export
```
Errors: `401 {error:"unauthenticated"}` with `WWW-Authenticate: Bearer realm="kudurru"`, `403 {error:"forbidden",
missing:{resource,verb,scope}}` (so the UI can explain), `429` with `Retry-After`.

## 9. What kudurru should borrow

1. **Technitium's permission algebra** — sections × {View, Modify, Delete} with per-item (zone) sub-permissions, user
   and group grants, built-in `Administrators`/`Everyone` groups, creator auto-grant on new zones, SSO group map with
   `allowSignup / onlyForMappedUsers / managedGroups`, `_FILE` env secrets, `hasDefaultCredentials` nag, API
   tokens that cannot change the owner's password, per-user session timeout, session listing with IP/UA.
2. **Headscale's identity key (`iss`+`sub`)**, tolerant claim parsing (string-or-array groups, string-or-bool
   `email_verified`), `allowed_{domains,users,groups}`, `client_secret_path` with env expansion, PKCE toggle, API key
   format `prefix-id-secret` with SHA-256 storage of high-entropy secrets and masked logging, unix-socket CLI.
3. **Kubernetes structured authentication config** as the schema for `claimValidationRules` / `claimMappings`
   (CEL via cel-go), issuer-unique routing of bearer JWTs, audience lists with `MatchAny`, and `TokenReview` audience
   verification for SA tokens; SA issuer discovery for JWKS-based verification.
4. **Gravity's** shared-store sessions (any replica serves any session), first-start admin/token bootstrap printed
   once, `?local` break-glass, JWT bearer acceptance for automation, redirect-URL placeholders per instance.
5. **Pi-hole's** cookie+CSRF discipline, app passwords that bypass TOTP for scripts, TOTP replay/rate protection,
   `max_sessions`, per-boot CLI password that cannot change config, constant-time comparisons.
6. **Grafana's** service accounts (multiple tokens, expiry caps, `None` role), `role_attribute_strict`, highest-role
   merge rule, `allowed_groups` / `allowed_domains`.
7. **PowerDNS-Admin's** two-level zone scoping (zone + account/zone-group), API keys carrying their own role and
   zone allow-list, per-zone history.
8. **RFC 10017 BFF**, **RFC 9700** (PKCE, exact redirect matching), **RFC 10027** warnings for the device flow,
   **kubelogin's** loopback/device/keyboard grant selection and keyring token cache.
9. **Go 1.25 `CrossOriginProtection`** and OWASP Argon2id parameters; `alexedwards/scs` as the session base.
10. **dnsdist's** API-key-only API (no Basic on `/api`), hashed keys, loopback-only default ACL, strict security
    headers — and model the dnsdist key as an integration credential kudurru provisions.

## 10. What kudurru should avoid

1. Giving every OIDC login full admin (Gravity) or defaulting unmapped SSO users to any role without an explicit
   `defaultRole`/`strict` setting.
2. Tokens in `localStorage` or non-HttpOnly cookies (Technitium SSO hand-off); implicit flow; access tokens in the
   browser at all (RFC 10017).
3. Storing API tokens in plaintext as lookup keys and returning them from list endpoints (Gravity); prefix-based
   session/token lookups or deletes (Technitium v15.6 oracle); bcrypt/Argon2 on 256-bit random tokens (CPU DoS).
4. Email as the identity key (changes, reused, unverified) — use `iss+sub`; also never trust `email` unless
   `email_verified` (Headscale).
5. HTTP Basic auth with the user password as the API credential (AdGuard, PowerDNS-Admin API) — it leaks the master
   secret into scripts and cannot be scoped or revoked individually.
6. A single global password (Pi-hole) or fixed three-role hierarchies with no zone scoping (PowerDNS-Admin
   decorators) — zone-scoped bindings are table stakes for a multi-team DNS server.
7. Binary/proprietary auth config files replicated by custom cluster RPC (Technitium) — keep auth state in the same
   DB/CRDs as everything else so k8s and Docker share one model.
8. URL-path-based permissions (Gravity `path`+`methods`) — they couple authorization to routing and cannot express
   "zone X only".
9. Cookies without `Secure` (AdGuard) and 365-day cookie lifetimes; sessions without idle timeout.
10. Network-hop authorization services (OpenFGA/SpiceDB) and a policy DSL (Casbin) as the *primary* model in v1;
    keep an `Authorizer` interface so they remain possible.
11. Logging raw usernames from login attempts without sanitising (Technitium log-injection fix) or user-controlled
    cost parameters (iterations) reachable via API.
12. Relying on `Sec-Fetch-Site` alone for CSRF when the UI may be served over plain HTTP in home labs — keep the CSRF
    header for cookie-authenticated writes and `SameSite=Lax`.

## 11. Open questions

- Should kudurru also act as an OP (issue its own OIDC tokens to integrations/dnsdist UI)? If yes, zitadel/oidc's
  `op` package becomes relevant; for v1 the answer is no.
- Query-log privacy: is `auditor`/`querylog.read` enough, or do we need per-client-group ownership (UniFi networks ->
  client groups -> who may see their queries)? Affects whether `clientgroup:` scope ships in v1.
- Cluster peer identity: mTLS with an internal CA vs shared join token (keepalived/dnsdist sidecars need the same
  answer). Leader election only for CRD reconciliation and token bootstrap — confirm with the k8s design.
- Multi-tenant namespaces: should `RoleBinding` CRDs in namespace A be allowed to reference zones from namespace B?
  Proposal: no; zones and bindings live together, cluster-scope bindings are `ClusterRoleBinding`-like and
  cluster-scoped.
- Back-channel logout and refresh-token-based group re-evaluation: v1 or v2? Proposal: refresh-based re-evaluation
  in v1 (cheap), back-channel logout v2.
- Pi-hole FTL is EUPL-1.2 (copyleft, verified); only matters if code is copied — none should be.
- Casbin's `go 1.13` directive and reflective matchers: benchmark before ever adopting as the production authorizer.

## 12. Source URLs

- Technitium: https://github.com/TechnitiumSoftware/DnsServer (APIDOCS.md, DnsServerCore/Auth/*, CHANGELOG v15.0/15.6);
  Authelia integration guide https://www.authelia.com/integration/openid-connect/clients/technitium/
- Gravity: https://github.com/BeryJu/gravity (pkg/roles/api/auth, docs/content/docs/api/{permissions,role_config}.md)
- AdGuard Home: https://github.com/AdguardTeam/AdGuardHome (internal/home/auth*.go, AGHTechDoc.md); Pomerium guide
  https://www.pomerium.com/docs/guides/ad-guard
- Pi-hole FTL: https://github.com/pi-hole/FTL (src/api/auth.c, 2fa.c, config/password.c, docs/content/specs/auth.yaml);
  https://docs.pi-hole.net/api/auth/
- PowerDNS-Admin: https://github.com/PowerDNS-Admin/PowerDNS-Admin
- dnsdist webserver: https://dnsdist.org/guides/webserver.html ; advisory 2017-02
  https://dnsdist.org/security-advisories/powerdns-advisory-for-dnsdist-2017-02.html
- Headscale: https://github.com/juanfont/headscale (config-example.yaml, hscontrol/oidc.go, hscontrol/db/secret.go)
- kubelogin: https://github.com/int128/kubelogin (docs/usage.md)
- go-oidc: https://pkg.go.dev/github.com/coreos/go-oidc/v3/oidc ; zitadel/oidc: https://github.com/zitadel/oidc ,
  https://pkg.go.dev/github.com/zitadel/oidc/v4?tab=versions ; x/oauth2: https://pkg.go.dev/golang.org/x/oauth2
- Casbin: https://pkg.go.dev/github.com/casbin/casbin/v3 ; OpenFGA/SpiceDB comparison
  https://docs.openedx.org/projects/openedx-authz/en/latest/decisions/0004-technology-selection.html
- Go CSRF: https://pkg.go.dev/net/http#CrossOriginProtection ; https://alexedwards.net/blog/preventing-csrf-in-go
- Kubernetes: https://kubernetes.io/docs/reference/access-authn-authz/authentication/ ;
  https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/ ; KEP-3331
  https://www.kubernetes.dev/resources/keps/3331/
- Grafana: https://grafana.com/docs/grafana/latest/setup-grafana/configure-security/configure-authentication/generic-oauth/ ;
  https://grafana.com/docs/grafana/latest/administration/service-accounts/
- Authentik RBAC: https://docs.goauthentik.io/users-sources/access-control/
- OWASP Password Storage Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
- RFC 9700 https://www.rfc-editor.org/info/rfc9700/ ; RFC 10017 https://www.rfc-editor.org/info/rfc10017/ ;
  RFC 10027 https://www.rfc-editor.org/info/rfc10027/ ; RFC 8628 https://www.rfc-editor.org/rfc/rfc8628 ;
  RFC 7636 https://www.rfc-editor.org/rfc/rfc7636 ; RFC 8252 https://www.rfc-editor.org/rfc/rfc8252 ;
  OIDC RP-Initiated Logout https://openid.net/specs/openid-connect-rpinitiated-1_0.html ;
  OIDC Back-Channel Logout https://openid.net/specs/openid-connect-backchannel-1_0.html
- cel-go https://pkg.go.dev/github.com/google/cel-go ; expr https://pkg.go.dev/github.com/expr-lang/expr ;
  scs https://pkg.go.dev/github.com/alexedwards/scs/v2
