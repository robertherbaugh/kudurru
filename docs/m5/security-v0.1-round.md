# Security review of the v0.1 round

Key `security-v0.1`. This is the second adversarial review, after
[docs/m5/security.md](security.md) (M5). It covers the attack surface added
since then:

- the block page listener (`internal/blockpage`);
- the unblock request form and the UnblockRequest API and approvals
  (`internal/unblock`);
- the Allowlist, Denylist, Domains and list-exception APIs and their
  migration code (`internal/domainlist`, `internal/api/domains.go`,
  `exceptions.go`);
- the catalogue runtime fetchers (`blocking.catalogue.url`,
  `blocking.catalogue.adguardServicesURL`) and the Block List Project
  content categories;
- the public catalogue icon routes;
- the list entries APIs, settings metadata validation, `/auth/me` and the
  password-change gate;
- the release pipeline workflows (`.github/workflows`).

The review started at 1ffaee7. Every finding below was reproduced before it
was fixed: by a test that fails at 1ffaee7 and passes after the fix commit,
or for the workflows (which cannot run here) by reading the actions'
documented behaviour. Live probes ran against `kudurru serve` built from
the round's head, with the block page on HTTP and HTTPS. Suspicions that
did not survive a reproduction are under [Checked and sound](#checked-and-sound)
or [Accepted risks](#accepted-risks).

## Threat model per surface

| Surface | Attacker | What they control | What must hold |
|---|---|---|---|
| Block page listener (`listeners.blockPage`, HTTP and HTTPS) | any device that can reach it, unauthenticated | Host header, TLS server name, path, method, headers, connection rate | no script, markup or header injection; no page for another device's queries; no proxying or fetching; bounded cost per client |
| Unblock request form (`POST /.kudurru/request`) | any such device; a web page in a victim's browser (CSRF, DNS rebinding) | the blocked name (Host), the form fields, the request rate | bounded writes to the store; no cross-site filing in a victim's name; recorded client as `querylog.privacy` allows; free text inert everywhere it is shown |
| UnblockRequest API and approvals | viewer, auditor, zone-admin, operator, scoped tokens, Kubernetes identities through SAR | approve and deny bodies, request IDs, timing (with the form) | no decision without `unblockrequests.decide`; approvals cannot be stretched beyond their scope or duration; no client address for readers without `unblockrequests.read` |
| Allowlist, Denylist, Domains, exceptions | the same principals | entry values, scopes, batch bodies, IDs | RBAC per route; managedBy respected without partial writes; bounded CPU per request |
| Catalogue fetchers | an admin (the URL settings); whoever controls or intercepts the upstream document | the document, redirects, DNS answers | no request to node-local or metadata addresses; bounded size and entry count; no rule that blocks every name |
| Catalogue icon routes (public) | anyone | the icon path | only embedded icons served, as `image/svg+xml` with a sandboxing CSP |
| Entries APIs | viewer (`blocklists.read`), operator (`blocklists.write`) | paging parameters, patch bodies | bounded CPU and memory per request and per node |
| Settings, `/auth/me`, password change | admin (settings), any authenticated user | PATCH bodies, path tricks, token scopes | hostOnly keys stay host-only; the must-change gate holds on every route; no disclosure beyond the caller's own principal |
| Release workflows | a collaborator with write access (branches, pull requests, workflow dispatch); a same-repository pull request | branch names, pull request contents, dispatch inputs | only the release branches build, sign and publish; signatures verify against that identity only |

## Findings

Severity is for a v0.1 deployment as documented; the attacker's starting
privilege is in the description. V1 to V4 concern the release pipeline,
the rest the server and dashboard.

| # | Severity | Surface | Finding | Reproduction (regression test or probe) | Fix |
|---|---|---|---|---|---|
| V1 | Medium | Release workflows | **A branch named like the next tag takes over publish.** `publish.yml` (and `docs-pages.yml`) checked out `${{ inputs.tag }}`. `actions/checkout` resolves a bare ref as a branch before a tag, and the next tag is predictable (`v<VERSION>`, `v<VERSION>-rc.<N+1>`). A collaborator who could create a branch `v0.1.0` made the release job build, test and sign their code with contents, packages, id-token and attestations write, and the token persisted in `.git/config` was readable by `make test`. | Reading `actions/checkout`'s ref resolution (branch first for a name without `refs/`); not executable outside GitHub. `actionlint` clean after the fix. | e2c4a83 |
| V2 | Medium | Release workflows | **The signing identity accepted any workflow of the repository.** Every documented `cosign verify` (release footer, docs/deploy, checklist) and the in-pipeline check used `--certificate-identity-regexp 'https://github.com/robertherbaugh/kudurru/'`. A same-repository pull request's own workflow with `id-token: write`, or `publish` dispatched from any branch with any tag value, produced signatures that passed. | Reading the Fulcio identity (`job_workflow_ref`) against the regexp; the new tag check exercised with `v0.1.0;id`, `v0.1`, `v01.0.0`, `v0.1.0-rc.0`, newlines (all refused). | 6898894 |
| V3 | Low | Release workflows | **Supervisor images built from a movable tag.** The supervisor-images job built `FROM ghcr.io/robertherbaugh/kudurru:<version>` and signed the result. Anything with `packages: write` (dev-image.yml for a same-repository pull request) could repoint that tag between the two jobs. | Reading the job; `actionlint` clean after the fix. | 012a9ba |
| V4 | Low | Release workflows | **The promotion check runs the pull request's own code.** `promotion.yml` is a `pull_request` workflow, so a pull request into `release` that edits it and `hack/release-tag.sh` passes its own check, and `release.yml` then runs the edited script. Branch protection as documented (pull requests and checks) does not stop it. | Reading the triggers. | f59800c (checklist: required maintainer review with CODEOWNERS; a repository setting, see [Accepted risks](#accepted-risks)) |
| V5 | Low-Medium | Catalogue fetchers, list fetcher | **A redirect through an HTTP(S) proxy reached cloud metadata.** With `HTTP(S)_PROXY` set, the guarded dialer only connects to the proxy, and `checkURLHost` ran on the first list URL only. A list server answering `302 Location: http://169.254.169.254/...` had the proxy fetch it, and the body was stored as a list (readable through the list search API, as in M5 S1). The catalogue refresher never called `checkURLHost`, so a literal metadata URL went to the proxy too, and its parse error leaked the first byte of the response in `GET /catalogue`. Attacker: a `blocklists.write` holder (operator) for lists, an admin for the catalogue URLs; only on nodes with a proxy. | `internal/listfetcher` `TestGuardedClientChecksRedirectsThroughProxy`: before the fix the proxy was asked for the metadata URL and Fetch succeeded. Live: BlockLists and `adguardServicesURL` pointing at `https://httpbin.org/redirect-to?url=http://127.0.0.1:…` and `…169.254.169.254…` are refused at the redirect. | 2389230 |
| V6 | Low-Medium | Catalogue fetchers | **An imported AdGuard service could block every name.** The import kept every rule the parser compiled: `\|\|com^`, `/.*/`, `\|\|arpa^`, `\|\|1.2.3.4^`. Imported services join kudurru's categories, so every Policy with that category in `blockedCategories` would block everything. Attacker: whoever controls or intercepts the configured file (an `http://` URL is accepted). | `internal/listdata/catalogue` `TestAdGuardRulesBlockOneSite` (12 match-all and malformed rules dropped, single-site rules kept); `FuzzParse` checks every kept rule. | a93c091 |
| V7 | Low | Catalogue fetchers | **The AdGuard import had no service count cap.** Only the 16 MiB byte cap applied: tiny entries gave about 315,000 services, and parsing, merging and compiling them took seconds and about 660 MiB on every node (replicas pull the same file). | `TestParseAdGuard`: 1001 entries are refused (before: accepted). | fa2f7c0 |
| V8 | Low-Medium | UnblockRequest API | **A viewer could recover a requester's address.** Request names were `ur-<sha256(client, domain)[:8]>`, and an approval records the name as the allow-list entry's `request`, which every `blocklists.read` holder (viewer, Kubernetes `view`) reads in `GET /allowlist`, `/domains` and the BlockList. Hashing the addresses of a /16 for the entry's name gave the client in milliseconds, although viewers may not read unblock requests or the query log. | `internal/unblock` `TestRequestNameIsKeyed`. Live: the viewer token reads `request=default/ur-…` from `/allowlist`; the name is no longer the unkeyed hash. | 642e336 |
| V9 | Low | Block page | **The block page showed other devices' blocks under pseudonyms.** Under `querylog.anonymize: pseudonym` the query-log finder matched the name alone, so any device that opened the block page for a name (`curl -H 'Host: …'` is enough) saw the list and rule of a block another device got for it in the last ten minutes, including names its own policy does not block. | `internal/blockpage` `TestQuerylogFinderPrivacy`: another device's pseudonymised block is not matched (before: matched); without a pseudonym nothing is. | 4e95a4a |
| V10 | Low-Medium | Approvals | **A re-submission made a time-limited approval permanent.** Approve wrote the allow entry, then the status. A re-submission from the block page between the two (the requester can trigger it, 5 per hour) changed the resource version, the status write failed with 409 and the entry stayed. The retry found the entry present (`added: false`), so the end of the approval never removed it. | `TestApprovalRacingAResubmission` (before: the approve failed with a conflict, and after the retry the entry outlived the 1 h approval); `TestFailedApprovalRemovesItsEntry`. | a2542d5 |
| V11 | Low | Approvals | **The end of an approval removed a manual entry with the same rule.** An operator who replaced an approval's entry with a permanent one of their own lost it when the approval expired or was denied. | `TestExpiryKeepsAManualEntry` (before: the list was empty after the sweep). | 7d5b4e6 |
| V12 | Low | Allowlist, Denylist | **Delete was a partial write when a copy was managed elsewhere.** For an entry with copies in `ui-allow` and a file-managed `ui-allow-<group>`, DELETE answered 409 `managed_by`, but the `ui-allow` copy was already deleted and the emptied list detached from its policies. `:batch` deletes take the same path. | `internal/domainlist` `TestDeleteWithAManagedCopy`. | 9410c02 |
| V13 | Medium | Entries APIs | **One operator PATCH could pin a core for tens of minutes.** `PATCH …/sources/{source}/entries` on an entries source parsed every entry's rule key again for every value: 1000 over 1000 took 13 s, 3000 over 3000 took 3 min 39 s, and the schema allows 10000 of each, with no cancellation. The exclude branch was quadratic too. | `internal/api` `TestSourcePatchIsLinear` (2000 values over 2000 entries and excludes: about 1.5 s with `-race`; minutes before). | 4f941f8 |
| V14 | Low-Medium | Exceptions | **List exceptions re-parsed the cached body once per value.** `POST /allowlist` or `/denylist` with a `list` (`blocklists.write`) opened and parsed every URL source's body for each of up to 1000 values: 1.8 s per value for a 60 MiB list. | `TestExceptionsParseEachSourceOnce`: one body open for five values (before: one per valid value). | 2f3a1e1 |
| V15 | Low | Entries APIs | **Unbounded concurrent parses of cached list bodies.** Every page of `GET …/sources/{source}/entries` (every viewer) parses the whole cached body: about 0.6 s of CPU and 76 MiB of allocations for a 60 MiB list; 32 concurrent requests allocated 2.4 GiB. | `TestBodyScanLimit`: a parse over the cap gets 503 with `Retry-After` (before: it ran). | 5352422 |
| V16 | Low | Approvals (API docs, dashboard) | **The approve dialog and API schema understated an everyone approval.** Both said it is attached to "the default policy and the requester's policies"; it is attached to every policy of the namespace kudurru may edit, strict and allow-only ones included. An approver reading the dialog could allow a name for clients they did not mean to. | `RequestsPage.test.tsx` pins the corrected text. | 0e8f103 |
| V17 | Info | Dashboard | **A catalogue category icon named `constructor` broke the services grid.** The icon map was indexed as a plain object; IDs allow `constructor`, which resolved to `Object`. Needs an admin-configured catalogue document. | `ServiceGrid.test.tsx` (before: `constructor` and `toString` failed to render). | bfc1b40 |

### Fix details

- **V1:** both publish checkouts and the docs-pages checkout use
  `refs/tags/<tag>` and `persist-credentials: false`. The release job
  needs no git credentials: goreleaser uses `GITHUB_TOKEN` through the API,
  helm and docker log in themselves.
- **V2:** the release job starts with a check that the tag is
  `v<MAJOR.MINOR.PATCH>` or `v<MAJOR.MINOR.PATCH>-rc.<N>` (N ≥ 1, as
  `hack/release-tag.sh` numbers them) and that `GITHUB_REF` is
  `refs/heads/release` or `refs/heads/release-candidate`; a hand-run
  publish therefore starts from a release branch. Every verification pins
  `^https://github\.com/robertherbaugh/kudurru/\.github/workflows/publish\.yml@refs/heads/release(-candidate)?$`.
- **V3:** the job resolves the tag to its digest with
  `docker buildx imagetools inspect`, runs `cosign verify` on that digest
  with the release identity and builds from `<image>@<digest>`.
- **V5:** `guardTransport` wraps the guarded client's transport and refuses
  a literal local host (`checkURLHost`) on every request it sends, the
  first and each redirect. It covers every user of
  `listfetcher.HTTPClient(false)`: the list downloader and both catalogue
  refreshers. The dialer still checks the resolved address of every
  direct connection.
- **V6:** `adguardRule` keeps `||<name>^` for a canonical host name that is
  not an IP address and not itself a public suffix
  (`golang.org/x/net/publicsuffix`, ICANN and private sections), so
  `||co.uk^` and `||github.io^` are dropped too. kudurru's own catalogue
  keeps its rule syntax.
- **V8:** `unblock.Name(key, client, domain)` is HMAC-SHA256 under
  `unblock.NameKey(encryptionKey)` (HKDF-SHA256, info
  `kudurru/unblock-request-name/v1`). Every node shares the encryption key,
  so every node names a client's request alike. After a key rotation, a
  client's next request for the same name is a new object; the old one
  expires as usual.
- **V9:** `querylog.Privacy.Pseudonym` and `querylog.Async.Pseudonym` give
  the name a client's queries are logged under today;
  `QuerylogFinder.Pseudonym` matches it in the live tail and querylog.db,
  and matches nothing without one. Under `anonymize: prefix` the finder
  still matches the /24 or /48, which the stored rows cannot narrow; the
  page documentation now says so.
- **V10:** on a conflict, Approve reloads the request and records the
  decision on it when its status is unchanged (nobody decided it
  meanwhile); otherwise, and on any other status error, it removes the
  entry it added before returning the error.
- **V11:** `domainlist.RemoveRule` takes an origin; releases remove only
  entries of origin `unblock`.
- **V13 to V15:** the entries branch keys the entries once into a map, the
  exclude branch keeps the excludes as a set, exceptions collect the rules
  of every value in one parse per source, and body parses (entries pages
  and exceptions) take one of `maxBodyScans` (4) slots per node, wait up to
  5 s for one, answer 503 with `Retry-After: 5` beyond that, and stop when
  their request is cancelled.

## Live probes

Against `kudurru serve` at the round's head (sqlite store, the block page on
HTTP and HTTPS, `blocking.defaultResponse: blockPage`), with API tokens
scoped to `viewer`, `auditor`, `operator` and `zone-admin@zone:example-com`.

- **Block page, hostile Host and SNI:** `<script>alert(1)</script>`, IP
  literals, `[::1]:80` and garbage give a page without a domain (and no
  form); `evil.com`, `com`, `_dmarc.x` and punycode render escaped; SNI is
  used when Host is not a name; `POST /` is 405, `OPTIONS` 405 with
  `Allow`; `/health` and `/robots.txt` as documented; `..%2f` paths render
  the page.
- **Unblock form:** cross-site `Origin`, `Origin: null`,
  `Sec-Fetch-Site: cross-site` and `same-site` are 403; same-origin is 303;
  a 9 KB body is 413; `text/plain` and `multipart/form-data` are 415; the
  sixth request from one client within the hour is `limited`.
- **Scope escalation:** viewer, auditor and zone-admin tokens get 403 on
  approve, `/allowlist` writes, `:batch` and `PATCH /settings`; an operator
  token gets 403 on `PATCH /settings` and on minting an `admin`-scoped
  token; approving for a group in another namespace is 422; path tricks on
  request IDs (`..%2f`, `%2e%2e`) are 403 or 404.
- **CSRF on the API:** cookie writes without the CSRF header are 401, with
  a cross-site `Origin` or `Sec-Fetch-Site` 403, including a `text/plain`
  deny and the password change.
- **Bodies and paths:** a 40 MB anonymous login and a 40 MB operator
  `/allowlist` body are 413; `/api/v1/../v1/settings` and `//settings` are
  307 to the clean path; `/catalogue/icons/..%2f..%2fsettings` and
  `%2e%2e%2fLICENSE` are 404; an icon is `image/svg+xml` with
  `default-src 'none'; style-src 'unsafe-inline'; sandbox` and `nosniff`.
- **SSRF:** BlockList URLs `http://127.0.0.1`, `[::1]`, `[::ffff:127.0.0.1]`,
  `0.0.0.0`, `[fe80::1%lo]`, `169.254.169.254`, `localtest.me` (resolves to
  `::1`), `127.0.0.1.nip.io` and `169.254.169.254.nip.io` (DNS answers for
  local addresses), and redirects to 127.0.0.1 and 169.254.169.254 are all
  refused with the `allowLocalListURLs` message; `2130706433`, `127.1` and
  `0x7f000001` do not resolve (pure Go resolver); `file://` and `gopher://`
  are refused as unsupported. `blocking.catalogue.url` on
  `127.0.0.1.nip.io` and `adguardServicesURL` redirecting to
  169.254.169.254 are refused the same way; a `file://` catalogue URL is
  400 at PATCH.

## Checked and sound

Each item was attacked, by reading the code and, where a test was cheap,
by trying it. None reproduced.

**Block page (`internal/blockpage`)**
- The blocked name is the Host header, else the TLS server name, and only
  ever a lower-case DNS name of letters, digits, `-` and `_` (IP literals
  and anything else give no name and no form). It is never a form field.
- Every value is HTML-escaped by `esc`, links pass `safeURL` (http, https,
  mailto). `FuzzBlockPage` (60 s, about 62,000 runs) found no element or
  attribute outside the template and no unsafe link for hostile hosts,
  settings, rules and form text.
- CSP `default-src 'none'`, the style by hash, `form-action 'self'`,
  `frame-ancestors 'none'`; no cookies, no script, no
  Strict-Transport-Security; `Referrer-Policy: same-origin`. The admin
  preview is the same HTML with `form-action 'none'` and `frame-ancestors
  'self'`, under `settings.read`.
- The client is the transport peer; forwarded headers are ignored. Rate
  limits per address or IPv6 /64, before the form and the reason lookup;
  `ReadHeaderTimeout` 5 s, `MaxHeaderBytes` 16 KiB. The listener never
  proxies or fetches.
- HTTPS: TLS ≥ 1.2, the self-signed pair in `<dataDir>/keys`.
- Under `full` the reason lookup matches the client address; under
  `hide_domains` and stricter modes no row is matched. The policy fallback
  evaluates only the requester's own policy.

**Unblock form and requests**
- Cross-site posts are refused by `Origin` and `Sec-Fetch-Site`; the form
  body is capped at 8 KiB before `ParseForm`; fields are length-capped and
  stripped of control and bidi characters; the honeypot stores nothing.
- Writes are bounded by the per-client limit, 30 per minute per node and
  `unblockRequests.maxPending`; groups and policies are capped at 16, the
  block rule at 1 KiB, the history at 20 decisions.
- The requested name is normalised again by `domainlist.Normalize` before
  it reaches a list: no `*`, `||`, `^`, `/`, spaces, line breaks or `@@`.
- Free text is not logged and is rendered as text by React and the block
  page. No notification channel carries it.

**UnblockRequest API, Allowlist, Denylist, Domains**
- Every route checks one permission at cluster scope: zone-scoped grants
  never cover them, token scopes narrow before bindings, peer nodes get
  viewer and auditor only. A 403 comes before any lookup, so there is no
  existence oracle.
- `viewer` cannot read requests; in the built-in roles only holders of
  `querylog.read` can.
- managedBy: only lists and policies owned by the API or bootstrap (or
  unowned) are written, bootstrap ones are taken over (D84), others are
  skipped and reported. `managedBy` cannot be claimed through a body.
- Migration touches only writable `ui-*` lists, is idempotent and keeps
  unconverted lines in `ui-unconverted`.
- IDs are base64url and split on a newline the namespace cannot contain;
  unknown IDs are 404. Values ≤ 1000, groups ≤ 100, batches ≤ 1000, source
  patches ≤ 10000 per array, the 32 MiB body cap and the store's object cap.

**Catalogue fetchers and icons**
- Both refreshers use `listfetcher.HTTPClient(blocking.allowLocalListURLs)`
  (after V5, with the request-URL check). Bodies are read through
  `LimitReader(cap+1)` on the decompressed stream and an over-cap body is
  an error, never a truncation: a 1 MiB gzip that decodes to 1 GiB stops
  at the cap in milliseconds. Brotli and zstd are not decoded.
- Replicas pull the leader's copy over the internal listener (mTLS, bearer
  token, per-kind cap, SHA-256 checked). Persisted copies have fixed names,
  mode 0600, temp file and rename, and are reused only for the same URL.
- `Parse` and `ParseAdGuard` bound services (1000), rules per service
  (1000), engines, categories, domains, text lengths and ID syntax; regex
  rules run on RE2. `FuzzParse` covers both parsers.
- No API route triggers a fetch; failures retry after 5 minutes; the URL
  settings are admin-only, need a restart and refuse non-http(s) schemes.
- Content categories make no fetch of their own: the dashboard creates
  ordinary BlockLists with fixed blocklistproject URLs, fetched by the list
  fetcher.
- Icons are looked up in the set embedded in the binary; no remote icon
  (AdGuard `icon_svg`) is stored or served. Path tricks, NUL, case and
  double extensions are 404.

**Entries APIs, settings, authentication**
- The entries cursor is base64 then `Atoi` (negative and overflowing values
  400), `limit` 0 to 1000, `q` a substring, `{source}` a name or bounded
  index; the cache key is SHA-256 of the spec URL, so no path reaches the
  disk cache.
- Only admin holds `settings.write` and `node.config`.
  `blocking.allowLocalListURLs`, `listeners.api.trustedProxies` and
  `listeners.blockPage.*` are `host_only` through either PATCH, nested or
  dotted. URL, contact and dashboard settings refuse `file:`, `gopher:` and
  `javascript:`. `tsigKeys` is redacted in GET, the schema and audit.
- The must-change-password gate is enforced server-side for the API routes
  (`authorizeMW`) and the forwarded auth routes (`auth.Authorize`), from
  the live account flag; sessions and pre-reset tokens get 403
  `password_change_required` everywhere but the self-service routes, and
  cannot mint tokens. `//`, `/./` and `/auth/../` redirect to the clean,
  gated path; `/API/v1/...` serves the SPA, no data.
- `/auth/me` shows the caller's own principal, groups, roles and CSRF token
  only, and no permissions while the flag is set. A password change needs
  the current password, goes through the login lockouts and revokes the
  other sessions.

**Release workflows (beyond V1 to V4)**
- Every workflow sets top-level `permissions: contents: read`; write scopes
  are per job. No `pull_request_target`, `workflow_run`, `issue_comment` or
  `secrets: inherit`; no secret besides `GITHUB_TOKEN`. Fork pull requests
  get read-only tokens and dev-image skips them; no privileged job consumes
  a pull request's artifact or cache.
- Untrusted values reach `run:` steps through `env:`; `zizmor`'s
  template-injection hits were checked one by one (`github.actor`,
  `matrix.target` from the repository's own script in a read-only job).
- Tags are pushed as `refs/tags/<tag>` without force, and
  `hack/release-tag.sh` refuses an existing `v<VERSION>`. cosign signs by
  digest everywhere. Downloaded tools are checked against SHA-256 or go
  through sumdb; there is no `curl | sh`.

## Accepted risks

Not fixed in this round. Each is documented, and the ones with a target are
in docs/followups.md.

- **`unblockrequests.decide` implies the approval's writes.** An approval
  writes `ui-allow` or `ui-allow-<group>` and attaches it to policies without
  checking the approver's `blocklists` or `policies` write; likewise the
  Allowlist API (`blocklists.write`) attaches `ui-*` lists and named lists
  to the policies (D143). The built-in roles grant these together
  (operator, admin); with custom Kubernetes roles `decide` is a delegated
  capability. docs/deploy/blocking-page.md now says so (0e8f103).
- **Shared requests.** Under `anonymize_client` and the hidden modes,
  devices share one request per name: each sees its state and the
  administrator's note, and a later post replaces the name, contact and
  reason. Inherent in recording less about the client; documented
  (aa53708).
- **Prefix-mode reasons.** Under `anonymize: prefix` the block page can
  show a device the reason of a block another device of its /24 or /48 got
  for the same name in the last ten minutes. Documented (4e95a4a).
- **API tokens survive a password change and an admin reset.** Sessions
  are revoked (`sessionsValidAfter`), tokens are not; an attacker who knew
  the password and minted a token keeps it. Disabling the account stops
  both. Follow-up: offer revoking the user's tokens with a reset.
- **A scoped token can call `POST /auth/password`.** The route is
  self-service; it needs the current password and goes through the login
  lockouts, also under `auth.localLogin: disabled`.
- **Names resolved by an HTTP(S) proxy.** Through a proxy, a host name that
  resolves to a local address is resolved by the proxy and cannot be
  checked by the node (literal addresses and `localhost` are refused, after
  V5 on every redirect too). Unchanged from M5 S1.
- **Catalogue URLs are not secrets.** `blocking.catalogue.url` and
  `adguardServicesURL` are shown in full to `settings.read` holders and in
  refresh errors, like BlockList URLs in their objects. Do not put
  credentials in them.
- **Block page limiter tables.** Each table tracks 16,384 clients and is
  reset when full and nobody is idle, so a client rotating through that
  many addresses resets everyone's buckets. The node-wide 30 writes per
  minute and `maxPending` still bound what the form writes.
- **Requests for names that are not blocked.** Any device can file a
  request for any Host, and a DNS-rebinding page can file one for its own
  name from a victim's browser. The request carries the block reason when
  one is found, and nothing is allowed without an approver.
- **Approving an existing entry overwrites its comment** with the
  approver's note, and an approval that ends an expired earlier approval
  first (`end`) and then fails to add its entry leaves the request marked
  approved. Integrity of comments and status only; follow-up.
- **Release hardening not done here:** third-party actions are pinned by
  tag (no Dependabot for actions), goreleaser, helm and cosign versions
  float, a release candidate is tagged before it publishes, images carry no
  build provenance, and `make test` runs in the privileged release job with
  setup-go's cache from `main`. Follow-ups.
- **The branch rulesets and reviews of V1 and V4** are repository settings
  this repository cannot carry; the release checklist lists them.

## Follow-ups added

See docs/followups.md, "Security (docs/m5/security-v0.1-round.md)".

## Shared files touched

| File | Change |
|---|---|
| `.github/workflows/publish.yml`, `docs-pages.yml`, `.goreleaser.yaml` | V1 to V3 |
| `docs/release-checklist.md`, `docs/deploy/README.md` | the signing identity, hand-run publish, rulesets and reviews (V1, V2, V4) |
| `cmd/kudurru/serve.go`, `cmd/kudurru/blockpage.go` | the pseudonym for the block page (V9), the request name key (V8) |
| `internal/querylog/privacy.go`, `async.go` | `Privacy.Pseudonym`, `Async.Pseudonym` (V9) |
| `internal/listfetcher/egress.go` | `guardTransport` (V5) |
| `internal/domainlist/service.go` | `RemoveRule` origin (V11), `readOnly` before Delete (V12) |
| `internal/api/api.go`, `sourceentries.go`, `exceptions.go` | V13 to V15 |
| `internal/api/unblock.go`, `api/openapi*.json`, `internal/apiclient/openapi`, `web/src/api/generated.ts` | the corrected approve scope description (V16) |
| `web/src/lib/unblock.ts`, `web/src/components/ServiceGrid.tsx` | V16, V17 |
| `hack/fuzz.sh` | `FuzzBlockPage`, `FuzzParse` (catalogue) |
| `docs/deploy/blocking-page.md`, `blocked-services.md`, `SECURITY.md` | the documented behaviour above |

### Architecture changes

For the integrator to record in §16:

- Unblock request object names are `ur-` and 16 hex digits of
  HMAC-SHA256(HKDF(encryption key, `kudurru/unblock-request-name/v1`),
  client and name). The alternative, hiding `request` from list readers,
  would not cover the BlockList object itself or Kubernetes `view`.
- The AdGuard import keeps only `||<name>^` rules below a public suffix and
  at most 1000 services; kudurru's own catalogue is unchanged.
- The guarded list client checks every request URL, redirects included,
  for literal local hosts.
- At most 4 concurrent parses of cached list bodies per node (503 with
  Retry-After beyond them after 5 s).
- Releases are published from the `release` and `release-candidate`
  branches only, and verified against
  `publish.yml@refs/heads/release(-candidate)`.

## Test and lint status

See [Results](#results).

### Results

At the round's head:

- `CGO_ENABLED=1 go test -race ./...` with envtest
  (`KUBEBUILDER_ASSETS` 1.37.0): all 72 test packages pass.
- `golangci-lint run --timeout 15m ./...` (v2.14.0): 0 issues.
- `go vet ./...`: clean.
- `make check-generated`: no drift (`make openapi`, `make client-go` and
  `bun run gen:api` were run for V16).
- `make e2e`: `test/e2e` and `test/interop` (BIND 9.18) pass.
- `cd web && bun run typecheck && bun run lint && bun run test`: clean;
  479 tests in 31 files pass.
- Playwright (`bun run e2e` against the real binary): 118 tests pass. In
  the first run 105 passed and 13 timed out waiting for pages while the VM
  ran at a load average of 85 (other test runs); those 13 pass on a re-run
  (`--last-failed --workers=1`), `requests.spec.ts` (block page request,
  approval, the name resolving) among them.
- Fuzzing: `FuzzBlockPage` 60 s (about 62,000 runs) and `FuzzParse`
  (catalogue) 45 s (about 165,000 runs), no failure.
- `actionlint` on the changed workflows: clean.
