# Spike (e): OIDC backend-for-frontend login against dex, sealed sessions, sessionsValidAfter

- Key: `auth-oidc`
- Date: 2026-10-11
- Branch: `worktree-wf_cd87f6f4-670-3` (fast-forwarded to the scaffold commit `79a0e37` first; the worktree started at the initial commit)
- Architecture: §8.1, §8.2, §8.4, §8.8 (Authenticate(cookie) + CSRF only); decisions D23, D48, D54, D57
- **Verdict: PASS.** The full BFF flow works against a real dex v2.46.0. A user in 200 IdP groups gets a **546-byte** session cookie, against a 4096-byte guard. Sealing a cookie costs about **2.8 µs** and opening one about **3.7 µs**. All 17 dex end-to-end subtests and all 31 mock-OP subtests pass under `-race`.

## What was built

| Package | Files | Contents |
|---|---|---|
| `internal/auth` | `grant.go`, `context.go` | The `Session.RolesSnapshot` wire format, a `Grant` written as `admin` or `zone-admin@zone:home-example-com`, plus `WithPrincipal`/`WithSession` context helpers |
| `internal/auth/session` | `sealer.go` | `Sealer`: derives a separate key per purpose with HKDF-SHA256 from the 32-byte master key (`session` and `oidc-preauth` today). Uses AES-256-GCM with a random 96-bit nonce (`cipher.NewGCMWithRandomNonce`). The envelope is `v1 \| keyID(4) \| nonce \| ct \| tag`, base64url without padding, and the header is authenticated as AAD. Seals with the current key and opens with the current or previous key (D57). Also `ParseKey` (base64 or hex). |
| | `sealed.go` | The `Sealed` `auth.SessionStore`: payload v1 `{v,sid,principal,roles_snapshot,idp,iat,exp,idle_exp,csrf}` (no groups, D48), idle (30m) and absolute (12h) expiry. `Touch` slides the idle expiry and re-seals at most once a minute, never past `exp`, always under the current key. A hard 4 KiB guard returns `ErrTooLarge`. The `ValidAfterFunc(principalID) time.Time` hook runs on every `Load`. `Revoke`/`List` return `auth.ErrUnsupported`. |
| | `cookie.go` | Cookies are always HttpOnly and SameSite=Lax. `Secure` is auto/always/never; auto means TLS terminates here, or `X-Forwarded-Proto: https` arrived from a trusted proxy (opt-in). |
| | `middleware.go` | `Authenticator.Middleware`, the cookie step of §8.8. It skips requests that carry `Authorization` (bearer requests are CSRF-exempt). It loads the session (seal, expiry, sessionsValidAfter) and clears bad cookies. On unsafe methods it enforces `http.CrossOriginProtection.Check` and the `X-Kudurru-CSRF` header (constant-time compare). It slides the idle timeout with a `Set-Cookie` and puts the session and principal in the request context. Also contains `SecurityHeaders`. |
| | `handlers.go` | `GET /auth/me` returns principal, roles, structured bindings, a permission matrix per scope, CSRF token and times. `POST /auth/logout` clears the cookie and is CSRF-protected. |
| `internal/auth/oidc` | `config.go` | `ProviderConfig`: id, issuer, clientID/secret, redirectURL, scopes, claimValidationRules, claimMappings and provisioning, with validation. https is required except for loopback hosts. `defaultRole` may never be `admin` and contradicts `mapped-only`. |
| | `claims.go` | The claim pipeline: validation rules, then the username chain (configured claim → `preferred_username` → `upn` → `email` if `email_verified` → `sub`), then groups from the groups claim plus `roles` (string or array) with a prefix. Group overage sent as a distributed claim is refused. `azp` and multiple-audience checks also live here. |
| | `identity.go` | `IdentityKey{provider, iss, sub}` written to `Principal.ID` as `oidc:<p>:<iss>:<sub>` (each part query-escaped). Email is never part of it. |
| | `resolver.go` | The `RoleResolver` interface and `BindingResolver` over `[]rbac.Binding`. A test pins it to `rbac.BindingAuthorizer`'s subject matching. |
| | `provider.go` | `Provider`, whose discovery is lazy and retried no more than every 10 s. `Start` seals a 10-minute pre-auth cookie `{state,nonce,verifier,next}` scoped to `/api/v1/auth/oidc/<idp>/` and redirects 302 with S256 PKCE and the nonce. `Callback` runs: state check → code exchange with the verifier → go-oidc `Verify` → nonce, `at_hash` and azp → claims → provisioning → `RoleResolver` → refuse if no binding → session cookie → 302 to a same-origin `next`. |
| | `registry.go`, `next.go`, `errors.go` | `Registry` rejects duplicate IDs and issuers and mounts `GET /api/v1/auth/oidc/{idp}/{start,callback}`. It fails if a `redirectURL` does not point at its callback route. `SafeNext` validates `next`. `LoginError` carries stable codes and user-safe messages. |

Test files: table tests for the grant format, sealer, store, middleware, claim pipeline, config, identity key, `SafeNext` and resolver, plus benchmarks. The end-to-end harness is `e2e_test.go`, `e2e_browser_test.go` and `e2e_suite_test.go`. `e2e_mock_test.go` runs it against an in-process OP built on go-oidc's `oidctest`, and `dex_test.go` runs it against real dex.

## How dex was run (repeatable in CI)

**Real dex** was used, not a mock. `go install github.com/dexidp/dex/cmd/dex@latest` **does not work** for two reasons. First, dex's `go.mod` has `replace github.com/dexidp/dex/api/v2 => ./api/v2`. Second, the module path has no `/v2`, so the proxy lists only ancient versions and `+incompatible` tags (`go list -m -versions github.com/dexidp/dex` stops at `v2.13.0+incompatible`). dex also publishes no release binaries; the release lists only container images. So dex is built from the pinned source tarball:

```sh
make test-auth-e2e      # builds bin/dex-v2.46.0 if missing, then runs the suite
```

which runs:

```sh
curl -fsSL -o bin/.dex-src/dex.tar.gz https://github.com/dexidp/dex/archive/refs/tags/v2.46.0.tar.gz
echo "3c09c0f2be88719d5bc2a061d3df9310052c8220b651a3b72d774eecd9b50b65  bin/.dex-src/dex.tar.gz" | sha256sum -c -
tar -xzf bin/.dex-src/dex.tar.gz -C bin/.dex-src --strip-components=1
cd bin/.dex-src && CGO_ENABLED=0 go build -trimpath -ldflags '-X main.version=v2.46.0' -o ../dex-v2.46.0 ./cmd/dex
CGO_ENABLED=1 KUDURRU_DEX_BIN=$PWD/bin/dex-v2.46.0 go test -race -count=1 -v -run 'TestDexEndToEnd|TestMockOPEndToEnd' ./internal/auth/oidc/
```

- The cold build took **21.6 s** (`real 0m21.619s`, go1.27.2, including module downloads); a rebuild with a warm module cache takes 2.2 s. The binary is 57 MB, static, `CGO_ENABLED=0`, and `dex version` prints `Dex Version: v2.46.0`.
- dex is an unmodified Apache-2.0 program, run as a separate process and never linked; `NOTICE` records it as test-only.
- Without a dex binary (`KUDURRU_DEX_BIN` unset and no `dex` on `PATH`), `TestDexEndToEnd` skips with a hint. `TestMockOPEndToEnd` always runs, so plain `go test ./...` still covers the whole flow.

For each run the test writes this config (as JSON) to a temp dir and starts `dex serve <config>` on a free loopback port. It then polls `<issuer>/.well-known/openid-configuration` until it gets a 200, which took 0.83–0.97 s across runs:

```yaml
issuer: http://127.0.0.1:<port>/dex
storage: { type: memory }
web: { http: 127.0.0.1:<port> }
logger: { level: info, format: text }
oauth2:
  skipApprovalScreen: true
  responseTypes: [code]
  pkce: { enforce: true, codeChallengeMethodsSupported: [S256] }   # dex refuses code flows without S256 PKCE
staticClients:
- { id: kudurru, secret: kudurru-e2e-client-secret, name: kudurru,
    redirectURIs: ["http://127.0.0.1:<kudurru-port>/api/v1/auth/oidc/dex/callback"] }
enablePasswordDB: true
staticPasswords:                       # bcrypt cost 10 (dex's minimum), generated per run
- { email: admin@example.com,  username: admin,  preferredUsername: admin,  emailVerified: true, userID: 6f1b4c2e-…-0001, groups: [kudurru-admins, staff] }
- { email: nobody@example.com, username: nobody, preferredUsername: nobody, emailVerified: true, userID: 6f1b4c2e-…-0002, groups: [staff] }
- { email: many@example.com,   username: many,   preferredUsername: many,   emailVerified: true, userID: 6f1b4c2e-…-0003, groups: [kudurru-viewers, org/departments/team-001-engineering, … 200 total] }
```

kudurru side: provider `dex`, username mapping `preferred_username` with prefix `dex:`, groups `groups` with prefix `dex:`. Bindings:

- `admin` ← Group `dex:kudurru-admins`
- `viewer` ← Group `dex:kudurru-viewers`
- `zone-admin@zone:home-example-com` ← User `dex:admin`

The fake browser is an `http.Client` with a cookie jar that never auto-follows redirects. It walks `/start` → dex `/auth` → `/auth/local` → `/auth/local/login`, parses the `<form action>` out of dex's HTML, POSTs `login`/`password`, and follows dex's redirects until one points back to kudurru.

## Gate and measurements

| Measurement | Value | Gate | Pass |
|---|---|---|---|
| Session cookie, 200-group dex user (`roles_snapshot=[viewer]`) | **546 bytes** (name=value) | < 4096 | yes |
| Session cookie, dex admin (2 grants, 106-byte identity key) | 592 bytes | < 4096 | yes |
| Session cookie, realistic unit case (long issuer, 3 grants, 200 groups at login) | 707 bytes | < 4096 | yes |
| Counterfactual: sealing the raw 200 groups (draft v1) | 9210 bytes | — | would fail; **D48 confirmed** |
| dex ID token for the 200-group user | 11344 bytes (admin: 1009) | — | shows why groups never go in a cookie |
| 4 KiB guard boundary | 68 zone-scoped grants fit, 69 return `ErrTooLarge` | hard error | yes |
| Seal (`Issue`: JSON + AES-GCM + base64), realistic session | 2.75 µs/op, 2849 B/op, 6 allocs (quiet run); 2.9–10 µs under the parallel-agent load | — | — |
| Open (`Load`: base64 + AES-GCM + JSON + checks) | 3.7 µs/op, 1393 B/op, 8 allocs (quiet); 3.9–9.4 µs under load | — | — |
| Open with the previous key (rotation window) | 3.7 µs/op (no trial decryption: the key id selects the key) | — | — |
| `Touch` re-seal | 2.9–3.0 µs/op (quiet) | — | — |
| dex end-to-end suite | 17/17 subtests, 2.4 s with `-race` | all assertions | yes |
| Mock-OP end-to-end suite | 31/31 subtests, 0.5 s with `-race` | — | yes |

Benchmark command and the quiet run. The machine has 8 CPUs, but GOMAXPROCS was 6 and other agents were running; load average was 5.5 during the later reruns, which explains their spread.

```
$ go test -run '^$' -bench . -benchtime=2s -count=3 ./internal/auth/session/
cpu: Intel(R) Xeon(R) Processor @ 2.90GHz
BenchmarkSeal-6              	  908889	      2745 ns/op	    2849 B/op	       6 allocs/op
BenchmarkOpen-6              	  659110	      3619 ns/op	    1393 B/op	       8 allocs/op
BenchmarkOpenPreviousKey-6   	  633550	      3734 ns/op	    1393 B/op	       8 allocs/op
BenchmarkTouchReseal-6       	  860266	      2932 ns/op	    2849 B/op	       6 allocs/op
```

Cookie sizes come from `go test -v -run 'TestCookieSize|TestSealedSizeGuard' ./internal/auth/session/` and from the `-v` output of `make test-auth-e2e`:

```
bench_test.go:57: 200-group user: roles_snapshot cookie = 707 bytes (limit 4096); raw-groups cookie would be 9210 bytes
sealed_test.go:320: 4 KiB guard: 68 zone-scoped grants fit, 69 do not
dex_test.go:168: dex …/bin/dex-v2.46.0 ready in 828ms at http://127.0.0.1:38337/dex
e2e_suite_test.go:68: session cookie for admin@example.com (2 grants): 592 bytes; principal id 106 bytes
e2e_suite_test.go:90: session cookie for a 200-group user: 546 bytes (guard 4096)
dex_test.go:229: many@example.com: id_token 11344 bytes, 200 groups, claims [at_hash aud auth_time c_hash email email_verified exp groups iat iss jti name nonce preferred_username sub], aud kudurru, refresh token issued: false
dex_test.go:261: dex without PKCE: invalid_request (PKCE is required. The code_challenge parameter must be provided.)
```

## End-to-end assertions (dex and mock OP both)

1. A login sets `kudurru_session` with HttpOnly, SameSite=Lax, no Secure over http (auto mode), Path=/ and Max-Age = 12 h. The pre-auth cookie is not visible outside its callback path.
2. `/auth/me` shows the principal (`Kind=User`, `ID=oidc:dex:<iss>:<sub>` parsed back into provider, iss and sub, `name=dex:admin`, `authMethod=cookie`). It also shows `roles=[admin, zone-admin@zone:home-example-com]`, the permission matrix and the CSRF token.
3. A user with no matching binding (`nobody`) gets `403 no_bindings`, a message naming the user and the group count that says "ask an administrator", and no session cookie.
4. The 200-group user fits in 546 bytes with `roles=[viewer]`.
5. Changing `state` on the callback gives `400 state_mismatch`. A callback delivered to a browser without the pre-auth cookie gives `400 missing_preauth`. Replaying a successful callback gives `400 missing_preauth`, because the pre-auth cookie is single-use.
6. Changing `nonce` in the authorization request gives `401 nonce_mismatch` (dex signs the attacker's nonce). Changing `code_challenge` makes dex reject the exchange: `401 exchange_failed`.
7. A cookie-authenticated POST with no header, or the wrong header, gets 403. With the right header it gets 200. With the right header plus `Sec-Fetch-Site: cross-site`, or a foreign `Origin`, it gets 403 from CrossOriginProtection.
8. Setting sessionsValidAfter to now + 1 h makes `/me` return 401 and clears the cookie. Once sessionsValidAfter is in the past, a fresh login works.
9. Idle timeout slides: activity at 20 m re-seals the cookie, the session is still valid at 40 m, the original cookie is rejected at 40 m, and 31 m without activity gives 401.
10. Absolute lifetime does not slide: activity every 25 m keeps the session alive until 12 h, then 401.
11. Rotation: with `current=B, previous=A`, an A-sealed cookie still opens, the next touch re-seals it under B, and a login started under A completes. After dropping A, the B-sealed cookie works and the original A cookie gets 401.
12. Logout without CSRF gets 403. With CSRF it gets 204, the cookie is cleared, and `/me` returns 401.
13. `next=//evil.example/x` and `next=https://evil.example/` both redirect to `/`; `/zones/home?tab=record` is kept.

Covered only by the mock OP (dex cannot be made to produce these):

- groups sent as a single string;
- `roles` read as groups;
- username falling back to `sub` when email is unverified;
- distributed groups claim refused (`distributed_groups`);
- multiple audiences without azp, a token for another client, and a foreign azp refused, while multiple audiences with our azp are accepted;
- `at_hash` mismatch refused;
- claim rule failure (`hd`) refused, then accepted once the claim is present;
- IdP `error=access_denied` reported;
- pre-auth cookie expired after 10 m;
- `defaultRole` with `autoProvision: always`;
- `autoProvision: never` with `IdentityExists`;
- an oversized role snapshot giving `500 session_too_large`;
- a resolver failure giving 500, not a login;
- the IdP being down at start giving `503 idp_unavailable` (lazy discovery).

## go-oidc gotchas (v3.21.0)

- **The nonce is not verified.** `IDToken.Nonce` is only exposed, so the caller must compare it (done, constant-time). The `oidc.Nonce()` auth-code option only *sends* it.
- **`azp` is not checked.** `Verify` only requires `aud` to contain the client ID; its own comment says it "DOES NOT ensure that the ClientID is the party to which the ID Token was issued". OIDC Core §3.1.3.7 rules 4–5 are implemented in `checkAudience`. This matters for the v0.2 bearer path (§8.3), which already specifies the rule.
- **`at_hash` is optional and left to the caller.** `VerifyAccessToken` errors when `at_hash` is absent, so call it only when `AccessTokenHash != ""`.
- **Distributed claims are not fetched.** If `_claim_names` references a source missing from `_claim_sources`, `Verify` fails. If it is well-formed, verification passes but `groups` is absent: Entra ID group overage would silently produce "no groups → no bindings". kudurru now detects it and refuses with a specific message.
- `IDToken.Claims` uses `encoding/json` with float64 numbers; re-decode with `UseNumber` for exact claim-rule comparisons. `email_verified` arrives as a string at some IdPs (go-oidc itself has `stringAsBool` for UserInfo only), so both forms are accepted.
- `Provider.Verifier` uses a background context for JWKS fetches. Use `VerifierContext(oidc.ClientContext(context.WithoutCancel(ctx), client), …)` so key refreshes use kudurru's HTTP client and outlive the triggering request.
- Signing algorithms default to discovery's `id_token_signing_alg_values_supported`; dex advertises RS256.

## dex gotchas (v2.46.0)

- `go install …@latest` cannot install dex (the replace directive and the `+incompatible` module path explained above), and there are no binaries, so build from the tarball. `dex version` prints `DEV` unless built with `-ldflags '-X main.version=…'`.
- Recent dex supports `groups`, `preferredUsername` and `emailVerified` on `staticPasswords`. That made the end-to-end test possible without the `mockCallback` connector or LDAP.
- The `groups` claim is a JSON array, also for one group, and is only present when the `groups` scope is requested. `aud` is a single string. dex also emits `at_hash`, `c_hash`, `auth_time` and `jti`.
- ID tokens live **24 h** by default (`exp − iat = 86400`). That is harmless for the BFF, which only uses the token at the callback. It matters for the v0.2 CLI bearer path, where §8.3 caches a verdict "until exp".
- dex issues no refresh token without `offline_access`. We do not request it (D23: no refresh token in the cookie).
- `oauth2.pkce.enforce: true` works, but the rejection is OAuth-style: after the connector-selection hop, dex redirects back to the client with `error=invalid_request`. It is not an HTTP error at `/auth`.
- dex's `info` log prints every login's full group list (all 200 groups on one line).

## Other surprises

- **`make test` is broken on the scaffold.** The Makefile `export`s `CGO_ENABLED=0`, so `go test -race` fails with `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`. CI calls `make test`, so the CI `go` job would fail. My `test-auth-e2e` target sets `CGO_ENABLED=1` for its test run. I did not touch the `test` target (out of scope); it needs the same fix.
- The worktree was created from `main` (the initial commit) without the scaffold. I fast-forwarded the branch to `79a0e37` before starting.
- Go 1.25 `CrossOriginProtection` allows unsafe requests that carry **neither** `Sec-Fetch-Site` nor `Origin` (non-browser clients, very old browsers). For cookie requests from such clients, the `X-Kudurru-CSRF` header is the protection that actually applies. Both layers are needed, as §8.2 says.
- Cookie jars (and browsers) do not separate cookies by port. dex and kudurru on the same host share the jar, so `kudurru_session` is also sent to dex. In production the same applies to an IdP on the same hostname. Path-scoping the pre-auth cookie limits it to the callback, and a `__Host-` prefix would help the session cookie (see recommendations).
- gosec flags `http.Cookie` with a computed `Secure` (G124), the nil nonce for the random-nonce AEAD (G407) and the `SafeNext` redirect (G710). Each is annotated with a one-line justification.
- `rbac.matchesSubject` matches a `User` subject with an empty name against a principal with an empty `Name`. `BindingResolver` guards against this; rbac should too.
- Picking `golang.org/x/crypto@latest` (v0.58.0, used for bcrypt in tests) bumped x/net, x/sys and x/text for the whole module. I pinned v0.57.0, which needs none of them, to keep the go.mod merge small.

## Known limitations (by design of D23/D54)

- **Logout cannot recall a sealed cookie.** A copied cookie stays valid until its idle or absolute expiry; a unit test asserts this so it stays visible. "Log out everywhere" means bumping sessionsValidAfter.
- **`roles_snapshot` is frozen at login.** Deleting a RoleBinding does not affect live sessions for up to 12 h unless sessionsValidAfter is bumped (see recommendation 1).
- sessionsValidAfter is compared at one-second resolution and fails closed: a login in the same second as a revocation is rejected and has to be retried.
- **Random 96-bit GCM nonces** limit a key to about 2³² seals. At one re-seal per active session per minute, that is tens of thousands of years for a home deployment.
- `provisioning.syncGroups` is accepted but has no effect, because there is no user store yet. `autoProvision: never` needs an `IdentityExists` hook from that store.

## Recommended changes to docs/architecture.md (not edited here)

1. **§8.2 / D48: specify the snapshot format and its staleness.** `roles_snapshot` entries are `role` or `role@kind:name`. Either have the RoleBinding controller bump `sessionsValidAfter` for affected subjects when it deletes or narrows a binding, or seal the *matched binding identities* instead of roles and intersect them with live bindings at Authorize time. The second option revokes instantly and is still bounded by the binding count.
2. **§8.2: write down the key schedule.** HKDF-SHA256(`KUDURRU_ENCRYPTION_KEY`, info=`kudurru/<purpose>/v1`) with purposes `session` and `oidc-preauth` (and later `secrets`), plus a 4-byte key id in the envelope so D57 rotation needs no trial decryption.
3. **§8.2: pre-auth cookie details.** Name it `kudurru_oidc_<idp>`, set Path=`/api/v1/auth/oidc/<idp>/`, make it single-use (cleared on every callback), and seal it under its own purpose. Use the `__Host-` prefix for the session cookie whenever `secure` resolves to true.
4. **§8.2: discovery is lazy, retried no more than every 10 s**, and a login during an outage returns `503 idp_unavailable`. kudurru must never fail to start because an IdP is down.
5. **§8.2 logout:** state explicitly that sealed logout only clears the browser's copy.
6. **§8.4: default the username prefix to `<idp id>:`**, or require it to be explicit. Otherwise an IdP account whose `preferred_username` is `alice` matches `User: alice` bindings meant for a local account.
7. **§8.4: document group overage.** A `_claim_names.groups` distributed claim is refused with a message telling the operator to emit only the groups assigned to the application (Entra ID). Also document that `defaultRole` may never be `admin` and contradicts `mapped-only`.
8. **§8.3 (v0.2 bearer): cap the verification cache TTL**, e.g. `min(exp, 15m)`, because dex's default ID token lifetime is 24 h.
9. **§12 / CI:** fix `make test` (`CGO_ENABLED=1 go test -race`) and add a CI job for `make test-auth-e2e`. It needs only Go and curl, takes about 25 s cold, and the dex binary can be cached by `DEX_VERSION`.
10. **§2 table:** add `github.com/go-jose/go-jose/v4` v4.1.5 (pulled in by go-oidc) and dex v2.46.0 as a test-only tool.
11. **§8.5:** export a subject-matching helper from `internal/rbac` (e.g. `RolesFor(principal) []Binding`) so `oidc.BindingResolver` stops duplicating it, and fix the empty-name match.
