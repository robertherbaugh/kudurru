# M5 work package: security review of auth and rendering

Key `security`. This is the adversarial review of docs/architecture.md
§13.1 M5 ("security review of auth + rendering") and risk #8 (§14). It
covers authentication, authorization, the REST API, the PROXYv2 and TSIG
code, the ingress rate limiter, the dnsdist and keepalived renderers, the
list fetcher and object loader, the query log, the internal listener and
the configuration surface.

Every finding below was reproduced with a failing test before it was
fixed. The test is named in the table, it fails on 347f4c7 and passes
after the fix commit. Suspicions that did not survive a reproduction are
listed under [Checked and sound](#checked-and-sound) or
[Accepted risks](#accepted-risks), not as findings.

The package also adds [SECURITY.md](../../SECURITY.md): the reporting
process, supported versions and the threat model summary.

## Findings

Severity is for a v0.1 deployment as documented: the attacker's starting
privilege is in the description.

| # | Severity | Finding | Reproduction (regression test) | Fix |
|---|---|---|---|---|
| S1 | Medium | **SSRF through block list URLs.** BlockList sources were fetched with `http.DefaultTransport`. An operator (any `blocklists.write` holder, not an admin) could point a source at `http://127.0.0.1:8083/` (dnsdist's webserver), the kubelet, a sidecar or `http://169.254.169.254/` (cloud metadata). The leader fetched it and stored the body. Status fields, rule counts and `GET /blocklists/_search` then act as an oracle for the response. | `internal/listfetcher`: `TestFetchRefusesLocalAddresses` covers literal loopback, `localhost`, `[::1]`, IPv4-mapped loopback, `0.0.0.0`, `169.254.169.254` and `fd00:ec2::254`; before the fix the local httptest server was reached. `TestGuardedDialerChecksResolvedAddress` covers names that resolve to loopback, the DNS-rebinding and redirect case. | aa02a40 |
| S2 | Medium | **A re-created local account inherits the old account's credentials.** Local principals are keyed by name (`local:<name>`), and LoadPrincipal re-read the live account by name. After a user was deleted, its personal API tokens (no expiry by default) and sessions (up to 12 h) came back to life when an account with the same name was created later. They then acted with the new account's rights. With `kudurru admin reset-password --create` the new account is a **builtin admin**, so an ex-user's old token became an admin token. | `internal/auth/stack`: `TestRecreatedUserDoesNotInheritCredentials`, on both store drivers. Before the fix the old token answered 200 with Decision.Path `local` (builtin admin). | f780092 |
| S3 | Medium | **Login lockouts counted the reverse proxy, not the client.** `serve.go` built the identity stack without `ClientIP`, so `POST /auth/login` used `RemoteAddr`. Behind `listeners.api.trustedProxies` (ingress, Traefik) every login came from the proxy address. Five failed logins by anyone locked **every** user out for 5 minutes, repeatably (availability). The per-client limit was also meaningless there. | `cmd/kudurru`: `TestLoginLockoutCountsClientsBehindTrustedProxy` wires the stack and the API server the way `serve` does. Before the fix the second client behind the same proxy got 429. | 2c17bc3 |
| S4 | Medium | **The unauthenticated external-dns webhook listened on all interfaces by default.** `externalDNS.webhook.listen` defaulted to `:8888`. Enabling the webhook (`externalDNS.webhook.enabled: true`) therefore exposed record writes in every primary zone, without authentication, to anyone who could reach the node (a LAN host in the Docker shape). The help text said to keep it on loopback, but the default did not. | `internal/config` `TestAPIDefaults` and `internal/integration/externaldns` `TestOptionsFrom` now pin the loopback default (before: `:8888`). | 3fd49cf |
| S5 | Low | **The built-in dig forged query-log rows.** `POST /api/v1/dns/query` needs only `stats.read` (every viewer) and runs a query as **any client address the caller names**. The log stage recorded it as that client's traffic: query log rows, top clients and per-client statistics. Under `anonymize_client` even the `api-dig` client name was dropped, so the rows could not be told from real ones. | `internal/stages/log` `TestObserverSkipsSyntheticRequests`; `internal/api` `TestDNSQueryIsSynthetic`. | 9c6ff88 |
| S6 | Low | **`hide_domains` kept the matched zone.** The `hide_domains`, `hide_domains_clients` and `nothing` modes replaced the query name, CNAME and rule but kept `Event.Zone`. The zone apex (for example `payroll.corp.example`) was stored in `dict_zone` and as the `zone:<apex>` upstream, a domain the mode promises never to keep. | `internal/querylog` `TestHideDomainsHidesZone`. | 9d8a467 |
| S7 | Low | **Unbounded live-tail streams.** `GET /queries/stream` accepted any number of concurrent streams, and each wrote with no deadline (`SetWriteDeadline(time.Time{})`). A `querylog.read` holder could open thousands of streams, or streams that never read, and pin goroutines, subscriber buffers and connections. | `internal/api` `TestQueryStreamLimit`: the stream over the cap got 200 before the fix. | 4f1c402 |
| S8 | Low | **Anonymous login bodies were buffered up to 32 MiB.** The forwarded auth routes inherited the 32 MiB object cap. huma read the whole body of an anonymous `POST /auth/login` into memory before the auth handler (64 KiB cap) refused it, so concurrent anonymous requests could push the node towards OOM. Authenticated routes are not affected: huma's authorize middleware answers 401 before reading any body (probed with a counting reader). | `internal/api` `TestForwardedAuthBodyCap`: the 1 MiB body reached the auth handler before the fix. | df0a127 |
| S9 | Low | **Unrestricted strings in the dnsdist configuration.** Backend and bind hosts were checked with `net.SplitHostPort` only. Certificate, key and DoH paths were not checked at all. A newline in a backend address ended the `-- backend <name> (<address>)` comment of `dnsdist.lua` and the rest ran as Lua. The `luaString` comment claimed that Validate restricts every rendered string, which was not true for these fields. The inputs come from `kudurru edge run` flags and EndpointSlices (the apiserver validates EndpointSlice IPs), so exploiting it needs control over the supervisor's arguments. | `internal/edge` `TestValidateRefusesUnsafeRenderedStrings` (newline, quote and space in hosts; newline and quote in paths; DoH paths). | 7bf1534 |
| S10 | Low (hardening) | **IPv6 login lockouts per /128.** The per-IP limiter keyed IPv6 clients by full address. One host owns a /64 and could rotate addresses to get 5 guesses per address; the per-user limit (10 per 15 min) still applied. | `internal/auth/users` `TestIPv6LockoutCoversThePrefix`. | 2c17bc3 |
| S11 | Info (hardening) | **Zone import cleanup ignored zoneRef.** A re-import deleted every api-managed record set in the namespace that carried the `kudurru.io/import=<zone>` label, whatever zone it belonged to. Exploiting it needs a writer of the other zone to set the label, so it is not a privilege crossing on its own, but the cleanup now also requires `zoneRef` to match. | `internal/api` `TestZoneImportKeepsOtherZones`. | 47d440f |

### Fix details

- **S1:** `listfetcher.DownloadOptions.AllowLocalAddresses` (config key
  `blocking.allowLocalListURLs`, node scope, hostOnly, restartRequired,
  default false). The default client checks every connection with a
  `net.Dialer` `ControlContext` on the **resolved** address, so redirect
  targets and DNS rebinding are covered as well. Literal local hosts and
  `localhost` are refused before any request; that also covers fetches
  through an HTTP(S) proxy, whose dial goes to the proxy. Dials to a proxy
  named by `HTTP(S)_PROXY` stay allowed.
  - Refused: loopback, link-local unicast and multicast, unspecified,
    `0.0.0.0/8`, multicast, `fd00:ec2::254` (AWS IMDS over IPv6) and
    `100.100.100.200` (Alibaba metadata).
  - Still allowed: private LAN ranges (RFC 1918, RFC 4193), where home
    users host lists.
  - The refusal is permanent (no retry) and its message names the
    opt-in. The existing list-fetcher tests now opt in because their
    httptest servers listen on 127.0.0.1.
- **S2:** `auth.Principal.CredentialIssuedAt` is set by the session step
  (iat) and the token step (token creation time).
  `users.LoadPrincipal` refuses a credential issued before the account's
  `creationTimestamp`, compared at one-second resolution because iat and
  creation timestamps are whole seconds.
- **S3:** `serve.go` now builds the stack options with `identityOptions`,
  which sets `ClientIP: apiClientIP`. That is the address of the API
  server's RealIP step (`api.ClientIP`), falling back to the transport
  peer for requests that did not pass the API server.
- **S4:** the default is `127.0.0.1:8888`. The chart already renders
  `:<port>` explicitly (behind its NetworkPolicy), so Kubernetes installs
  do not change. A listener beyond loopback logs a warning at start.
- **S5:** `pipeline.Client.Synthetic`. The API's dig sets it and the log
  stage skips it, while metrics still count the query.
- **S7:** at most 64 concurrent streams per node (`maxQueryStreams`).
  Further streams get 503 `unavailable` with `Retry-After: 5`, and every
  write cycle (events, flush, heartbeat) has a 30 s deadline.
- **S9:** `validHostPort` requires an IP address or a DNS name
  (`[A-Za-z0-9._-]`), and backends' internal addresses are validated too.
  Certificate, key and DoH paths must be printable ASCII without spaces,
  quotes or backslashes.

## Checked and sound

Each item was attacked, by reading the code and, where a test was cheap,
by trying it. None reproduced.

**Sealed sessions (`internal/auth/session`)**
- AES-256-GCM with `NewGCMWithRandomNonce`: a fresh 96-bit nonce on
  every seal, so no nonce reuse at cookie volumes.
- Keys come from HKDF-SHA256 per purpose (`session`, `oidc-preauth`), so
  a pre-auth cookie never opens as a session. The 4-byte key id and the
  version byte are authenticated as additional data. An unknown key id is
  `ErrInvalid` without a trial decryption, and the previous key opens and
  is re-sealed under the current one on Touch.
- Load checks absolute expiry, idle expiry and `sessionsValidAfter`, with
  the iat ≤ validAfter case failing closed. Touch never extends past exp.
- The 4 KiB guard covers the `__Host-` name. Cookies are HttpOnly and
  SameSite=Lax, and `__Host-` plus Secure are used under TLS (or behind a
  trusted proxy reporting https).
- On a CSRF failure the session is dropped, not failed: public routes run
  anonymous and protected routes answer 401/403. Logout refuses on a CSRF
  failure. CSRF tokens are 256-bit and compared in constant time.
  `http.CrossOriginProtection` runs on every unsafe request before
  authentication.

**API tokens (`internal/auth/tokens`)**
- The full SHA-256 of the wire token is compared in constant time, and
  an unknown id is compared against a dummy hash. Unknown id, wrong
  secret, malformed, expired, and owner missing or disabled all give the
  same 401. There is no prefix lookup.
- Scopes narrow every decision (Engine step 1), including
  `?all=true` and deleting other users' tokens.
- A token-authenticated caller cannot mint a token with broader scopes
  (exact subset) or a longer life than its own token.
- OIDC owners' frozen grants stay subject to `BindingSource.GrantValid`.
- A bearer credential that no step claims is 401, never anonymous, and a
  bearer request never falls back to the cookie.

**Local users (`internal/auth/users`)**
- Argon2id m=19 MiB, t=2, p=1 with a global hashing semaphore. PHC
  parameters are bounded, so a tampered hash cannot force gigabytes of
  memory.
- The 1 KiB password cap applies before hashing, and over-cap passwords
  are refused without hashing for known and unknown users alike.
- Unknown users and users without a password verify against a dummy
  hash.
- Lockout keys use the same normalization as the lookup
  (`ToLower(TrimSpace)`). Unicode case folding (Kelvin sign `K`,
  Turkish `İ`) either maps to the same key as the account or to a
  non-existent account, so there is no bypass.
- A correct password does not reset the IP counter. Usernames are
  sanitized in logs.

**OIDC (`internal/auth/oidc`)**
- Authorization code with PKCE S256. state and nonce are 256-bit random
  values compared in constant time. The pre-auth cookie is sealed,
  single-use (cleared first), bound to the IdP id and expires after 10
  minutes.
- `SafeNext` refuses schemes, hosts, `//`, `/\`, control characters and
  values over 2 KiB. `http.Redirect`'s `path.Clean` cannot rebuild `//`
  from a value that passed (checked `/./`, `/../`, `%2F%2F`, `/%5C`).
- Callback: go-oidc checks iss, aud ∋ clientID, exp and the signature.
  kudurru adds the checks for several audiences without azp, an azp that
  is not ours, at_hash, and an empty sub.
- Bearer ID tokens:
  - aud must contain cliClientID, clientID or a configured audience;
    several audiences need a kudurru azp;
  - `typ: at+jwt` is refused;
  - verdicts are cached by sha256(token) for min(exp, 15 m);
  - routing by unverified iss happens only after the 64 KiB cap.
- Group overage (`_claim_names`) is refused instead of being read as no
  groups. Email is used only when `email_verified`.
- `autoProvision: never` is refused in v0.1. defaultRole may not be
  admin.

**Kubernetes identities (`internal/auth/k8s`, `internal/rbac/sar.go`)**
- Projected tokens are verified against the cluster's JWKS, fetched
  through the apiserver with the pod's credentials (no jwks_uri from an
  attacker), with audience `kudurru` and an exact
  `sub == system:serviceaccount:<ns>:<name>`.
- TokenReview runs only while discovery fails. It requests the audience,
  checks it in the answer and accepts ServiceAccount usernames only.
- SAR is keyed by (user, uid, groups, resource attributes), allows for 2
  minutes and denies for 30 s, and an outage means deny.
- `auth.kubernetes.mode: impersonate` is refused (D55).

**Admin socket (`internal/auth/adminsock`)**
- Peers are checked with `SO_PEERCRED` on every request: uid 0, the
  server's own uid or the configured gid. Connections without credentials
  are never the local root, and the TCP listener carries no peer
  credentials.
- The socket is mode 0660 in a 0750 directory. A non-socket file at the
  path, or a live socket, is refused rather than removed.

**RBAC (`internal/rbac`)**
- First allow wins, after scopes narrow.
- `Scope.Covers` gives cluster everything, otherwise kind and name must
  match exactly. A zone grant never covers a cluster check.
- Wildcards: `*.*` only in admin; resource-level `x.*` only for verbs of
  that resource.
- Subject matching never matches empty names.
- Restrictions and the `require` shortcut cannot widen: a cluster-scope
  allow already passed the restriction at cluster scope.

**REST API (`internal/api`)**
- Every route declares a permission and the lint enforces it.
- Zone scoping:
  - zone-admin cannot create zones (zones.write at cluster scope);
  - `zoneName` and `zoneRef` are immutable (CRD transition rules), so a
    record set cannot be moved into or out of a zone, and a zone-admin
    cannot rename a zone to take over another domain;
  - endpoints outside the zone apex are refused by the zone builder;
  - URL and body namespace and name must agree in `writeObject`, and
    `authorizeWrite` resolves them the same way;
  - reads of missing objects check the guessed scope first, so there is
    no existence oracle.
- `managedBy` cannot be claimed through the body (stripped), and writes
  and deletes of other managers' objects are 409.
- `If-Match` is checked against the stored resourceVersion, and the
  store re-checks with `ExpectedResourceVersion`.
- Settings:
  - hostOnly keys are refused by PATCH;
  - writeOnly keys (`tsigKeys`) are redacted in GET, the schema and
    audit;
  - IdP secrets are files or Secret references only, never inline.
- Security headers are set on every response. HSTS is set only when TLS
  terminates here or a trusted proxy says https. `X-Request-Id` and
  `X-Forwarded-*` are believed only from trusted proxies, and
  `X-Forwarded-For` is read right to left.
- Self-signed certificates: P-256, the key written 0600 in a 0700
  directory, TLS ≥ 1.2.
- The UI handler serves an embedded FS after `path.Clean`, so there is
  no traversal, and `/api/*` never falls back to index.html.
- With a path prefix, requests outside it are 404. Authentication runs on
  the cleaned `/api/v1/` prefix, and a request that skips it is
  anonymous and refused (fail closed).

**DNS server**
- PROXYv2 (`internal/dnsserver/proxyproto`): length checks before every
  slice, TLV walk bounds, untrusted peers refused before parsing,
  constant-time secret compare, LOCAL and AF_UNSPEC authenticated too,
  fuzz test present.
- TSIG (`internal/dnsserver/tsig.go`):
  - `hmac.Equal` with length mismatch failing, so truncated MACs are
    refused;
  - the algorithm is bound to the key;
  - the time check follows RFC 8945 (fudge from the signed request),
    with BADTIME carrying the server time;
  - in-window replay is inherent to TSIG and bounded by the fudge (300 s
    for kudurru's signatures).
- Ingress rate limiter (`internal/stages/ingress`):
  - it is keyed by the PROXYv2-attributed client and limits only UDP;
  - spoofed sources can drain a victim prefix's bucket (inherent), and
    the 50 % TC slip lets real clients retry over TCP;
  - LRU eviction under a spoofed flood resets buckets, which only
    restores the configured burst.

**Edge renderers**
- keepalived (`internal/edge/keepalived`): every rendered token is
  `safeToken` or `safePath`, peers and the unicast source are parsed IP
  addresses, the probe command elements are checked, and HMAC keys are
  `file:` references.
- dnsdist (after S9):
  - YAML is produced by the yaml.v3 encoder;
  - Lua strings use `%q` over printable ASCII;
  - names, zones, the server id, API key and password are `safeName`;
  - the console and webserver must be loopback;
  - the PROXYv2 secret is printable ASCII without quotes;
  - configuration files are written 0600.

**Objects and list fetcher**
- `objects.d` files are capped at 64 MiB. The k8s YAML decoder rejects
  alias bombs.
- List bodies are capped (Content-Length checked first, then a
  `LimitReader` on the decompressed stream, so a gzip bomb is cut at the
  cap) and validated as lists.
- Disk cache names are hex(sha256(url)). `GET /internal/lists/{digest}`
  accepts only 64 lowercase hex characters and serves only digests the
  cache recorded, re-checking the digest on open.
- `configMapRef` sources are unsupported until a ConfigMap reader is
  wired, so there is no cross-namespace read.

**Query log**
- Every filter value is a bound parameter. `LIKE` patterns are escaped,
  and `ORDER BY` and `HAVING` strings are package constants.
- Privacy is applied before the live tail and before storage. Pseudonyms
  use a random in-memory HMAC key that rotates daily and is never
  persisted.

**Internal listener (`internal/cluster/internalapi`)**
- TLS 1.3 only on server and client.
- Node certificates come from an Ed25519 CA derived with HKDF from the
  encryption key.
- The bearer token is HKDF-derived with purpose separation and compared
  in constant time against current and previous keys.
- Request bodies are refused, headers are capped, and there is a
  per-address token bucket and a download semaphore.
- Peers that present the token get viewer and auditor only.

**Configuration**
- `kudurru config show` and the settings API redact writeOnly keys, and
  key material only ever comes from files or the environment.

## Accepted risks

Not fixed in M5. Each is documented, and the ones with a target are in
docs/followups.md.

- **OIDC usernames from `preferred_username`.** The default username
  chain is `preferred_username → upn → email (verified) → sub`, prefixed
  `<idp>:`. Where an IdP lets users edit `preferred_username` (some
  Keycloak and Authentik setups), a user can take over another user's
  `User` bindings. Group bindings, or `claimMappings.username.claim: sub`,
  avoid this. It is the §8.4 design (the Kubernetes model); a
  documentation warning is the follow-up.
- **ServiceAccount binding subjects match both kinds.** A binding subject
  `{kind: ServiceAccount, namespace: N, name: X}` matches a kudurru
  service account `N/X` **and** a Kubernetes ServiceAccount `N/X` (§8.5
  example). A cluster user who can create ServiceAccounts and mint tokens
  in N gets the bindings meant for the kudurru service account of that
  name. builtin-admin is a property of the kudurru object and does not
  transfer.
- **Per-replica state.** Login lockouts and binding-revocation times are
  per-replica in memory (existing follow-ups).
- **The external-dns webhook has no authentication** (contract limitation).
  It is now loopback by default; a shared-secret header option is a
  follow-up.
- **TokenReview fallback amplification.** While Kubernetes discovery is
  failing, each distinct unauthenticated JWT costs one TokenReview, with a
  10 s negative cache.
- **`listeners.api.trustedProxies` and X-Forwarded-Proto.** The cookie
  Secure decision reads `X-Forwarded-Proto` from any peer when trusted
  proxies are configured, while HSTS checks the peer. A forged header only
  changes the attacker's own cookie name and flags.

## Follow-ups added

See docs/followups.md, "Security (docs/m5/security.md)":

- zone-admin-only principals get 403 on `/auth/tokens`;
- token `lastUsedIP` records the proxy address;
- a shared-secret option for the external-dns webhook;
- a username-claim warning in the OIDC docs.

## Shared files touched

All changes are small and additive. The new config key and the
`Principal` and `Client` fields are additions; the only behaviour changes
are the two defaults (S1 and S4) and the stricter edge validation (S9).

| File | Change |
|---|---|
| `cmd/kudurru/serve.go` | `identityOptions` and `apiClientIP` (S3); `AllowLocalAddresses` from `blocking.allowLocalListURLs` (S1) |
| `internal/config/schema.go`, `internal/config/blocking.go` | new key `blocking.allowLocalListURLs` (S1); `externalDNS.webhook.listen` default `127.0.0.1:8888` (S4) |
| `internal/config/api_test.go`, `internal/integration/externaldns/externaldns_test.go` | the pinned webhook default follows S4 (the assertion is unchanged in strength) |
| `internal/auth/auth.go` | `Principal.CredentialIssuedAt` (S2) |
| `internal/pipeline/pipeline.go`, `internal/stages/log/log.go` | `Client.Synthetic` and its skip in the log stage (S5) |
| `internal/edge/topology.go` | stricter `Validate` (S9) |
| `internal/listfetcher/*_test.go` | fixtures that fetch from httptest on 127.0.0.1 set `AllowLocalAddresses: true` |

## Test and lint status

`CGO_ENABLED=1 go test -race ./...` with envtest, `golangci-lint run` on
the touched packages and `go build ./...`: see the
[result section](#results) below.

### Results

Filled in at the end of the package.
