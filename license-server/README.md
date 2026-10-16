# kudurru license portal

Copyright 2026 Robert Herbaugh. The project-controlled source is available
for inspection under the [Kudurru License Server Source-Viewing License](LICENSE.md).
Building, modifying, testing by execution, running or redistributing it
requires prior written authorization. A Kudurru community or commercial
license does not grant rights to operate this service. The development and
deployment instructions below are for authorized maintainers.

The licensor's service that issues kudurru license keys: the Ed25519
signed tokens of `internal/license` (`docs/architecture/licensing.md`).
People sign in with OpenID Connect. Customers see and download their keys
and ask for a free personal key; license admins issue, renew, change and
withdraw licenses, approve personal keys and answer business requests;
superadmins manage and delete users and see the audit trail and the
configuration. The kudurru.io form requests free personal keys and sends
business requests without an account. By default
(`PERSONAL_AUTO_APPROVE=false`) no personal key is signed on its own: each
one waits until a license admin approves it, usually with one click from a
mail ("Approving personal keys" below).

The portal issues and delivers keys and has a Downloads page for every
signed-in user. Downloads links to public GitHub Releases and GHCR packages
(Helm charts, container images, Compose bundles, binaries, Linux packages
and Kubernetes manifests), with pull commands and deployment guides. The
links assume the repository and packages are public; versioned archives
are selected on GitHub's release page. Files are downloaded directly from
GitHub, and the portal needs no GitHub credentials or release API calls.

No kudurru node ever contacts it: keys are verified offline on each node
against the public keys built into the binary (D163), and licensing never
touches DNS.

It is Bun, TypeScript and Postgres in one container, with a React and
Astryx UI that uses the same design system and Kudurru theme as the
dashboard. The working name in the repository is "license server"; the
product is "kudurru license portal".

```sh
make license-server-check   # typecheck, lint, server and UI tests, build (TEST_DATABASE_URL for the database tests)
make license-server-up      # the image with Postgres, Dex and Mailpit on http://localhost:8791/
make license-server-e2e     # the Playwright suite against a fresh compose stack
make license-server-dev     # server and Vite on the host (http://localhost:5174/), compose for the rest
```

Or inside `license-server/`: `bun install`, `bun run dev`, `bun run test`,
`bun run build`, `bun run lint`, `bun run typecheck`, `bunx playwright test`.
Development users and secrets: [dev/README.md](dev/README.md). How it
works, with diagrams: [docs/architecture.md](docs/architecture.md).
Hosting on Cloudflare Containers: [cloudflare/README.md](cloudflare/README.md).

## Layout

| Path | What it is |
|---|---|
| `shared/` | What server and UI share: the wire types (`api.ts`), the route manifest (`routes.ts`, the one list of routes with their access rule), roles and permissions, the plan and feature catalog (checked against `internal/license/license.go` by a test) and UTC date helpers |
| `server/main.ts` | The entry point: `serve`, `migrate`, `grant-superadmin`; `server/healthcheck.ts` is the image's health check |
| `server/http/` | The router built from `shared/routes.ts`, the request pipeline (client address, security headers, CORS, CSRF, rate limits), body validation, the static UI |
| `server/auth/` | OIDC sign-in (openid-client), sessions in Postgres, identity and role mapping, member linking |
| `server/licensing/` | The JWS signer that matches `license.Sign` byte for byte, the keyring and the issuing policy |
| `server/services/`, `server/api/` | Customers, members, licenses, personal and public key requests, business requests, users, the block list, audit and system pages; `api/` holds the thin handlers |
| `server/mail/` | SMTP (nodemailer), mail texts and the shared branded HTML layout |
| `server/migrations/` | The schema; `0002` grants the runtime role exactly what the code uses (`0003` adds the approval links' table, `0004` deleting users, `0005` the business requests' table, each with its grant) |
| `server/test/` | `bun test` against Postgres; the Go golden vectors and node oracle in `golden/` |
| `ui/` | The UI (Vite, React 19, Astryx 0.6.8); `ui/src/ui/` is the kit copied from `web/src`, each file marked with where it came from |
| `e2e/` | Playwright: every role, the kudurru.io form and the one-click approval through Dex and Mailpit; every key handed out is judged by the node's own code |
| `deploy/postgres-roles.sql` | The one-time database role setup |
| `cloudflare/` | The Cloudflare Containers deployment (a package of its own) |
| `Dockerfile`, `compose.yaml`, `dev/` | The image, the development and e2e stack, its Dex configuration |

## Who signs in

Customers and staff sign in at the same OIDC issuer. Pick one that lets
customers register (or be invited) and that verifies email addresses
before it asserts `email_verified`, for example Zitadel, Keycloak or
Authentik with registration and email verification on, or Auth0. A
workforce-only IdP (Google Workspace, Entra for the team) shuts customers
out; the admin fallback then is "Email key" from a license's page.

- **Roles.** `customer`, `license_admin` and `superadmin`. A role comes from
  IdP groups (`ROLE_*_GROUPS`, read only from `OIDC_GROUPS_CLAIM`) or is
  granted in the portal by a superadmin; a sign-in that maps to nothing
  gets `DEFAULT_ROLE` (`customer`, or `none` to refuse it). superadmin can do
  everything a license admin can, and both everything a customer can.
- **Admin groups at a public IdP.** With `DEFAULT_ROLE=customer` anyone the
  IdP knows signs in, and on some IdPs outsiders can name their own groups.
  The server refuses to start with admin group mappings unless
  `OIDC_ADMIN_CLAIM_RULES` restricts them (for example `hd=kudurru.io` or
  `tid=<tenant id>`). With a self-registration IdP, grant admin roles in the
  portal instead.
- **Customer access.** A license admin adds a member to a customer; the
  person gets a one-time invite link (14 days, also mailed) and accepts it
  signed in. An IdP's `email_verified` alone never grants access, unless you
  set `MEMBER_AUTO_LINK=true` for an IdP that verifies every address itself.
- **The first superadmin.** Sign in once, then on any machine that reaches
  the database (only the database settings are needed):

  ```sh
  docker run --rm -e DATABASE_URL='postgres://…' ghcr.io/robertherbaugh/kudurru-license-server:<version> grant-superadmin --email you@example.org
  ```

  `--issuer` and `--subject` pick one user when an address is ambiguous;
  `--revoke` takes the role away. It also recovers access when the last
  superadmin is gone; the API never lets the last one lose the role.

## Keys

- **Format.** A compact JWS, header `{"alg":"EdDSA","kid":…,"typ":"JWT"}`,
  claims in `license.Claims` order, Go's HTML-safe escaping: identical to
  what `hack/licensegen` signs (`server/test/jws.test.ts` reproduces Go's
  golden vectors byte for byte).
- **Rules the node cannot enforce.** A key never lists `features: []` (that
  would mean every feature) and never carries `nbf` (a renewal installed
  early would otherwise drop the node to the free limits). A personal key
  covers up to 3 nodes and every feature. Every key is valid for more than
  a day from now; `exp` is 23:59:59 UTC of the chosen date, so `kudurru
  license status` prints the same date the portal shows. Nodes keep
  accepting a key for 30 days after it expires.
- **Withdraw cannot stop a key.** It hides the key from the customer, blocks
  renewal and cancels open requests. The person's later personal requests
  wait for a signed-in license admin (their mail has no one-click link).
  Installed keys keep working until their expiry plus 30 days, and the
  dialog says so. Stopping keys everywhere means rotating the signing key
  (below).
- **Keys signed by hand.** "Import key" records a key `licensegen` signed
  before the portal existed, after verifying it against the trusted keys.

### The signing key

The private key never enters the database or the image. Give it as a file
(`LICENSE_SIGNING_KEY_FILE`, licensegen's format: standard base64 of the
32-byte seed) where secrets are files, or as `LICENSE_SIGNING_KEY` where the
platform passes secrets as environment variables only (Cloudflare
Containers). `LICENSE_SIGNING_KID` names its `<kid>.pub` in
`LICENSE_TRUSTED_KEYS_DIR`, which the image fills from
`internal/license/keys` at build time. The server refuses to start when the
kid is not trusted, when the seed does not derive the trusted public key,
or (outside dev mode) when it is a test key. Without a key it runs with
signing unavailable: issuing answers 503 and `/readyz` says why;
`REQUIRE_SIGNING_KEY=true` makes that fatal.

Code execution in the container means unrecallable forged keys, so keep
the container lean: no shell (distroless), read-only root, no capabilities,
no core dumps (`compose.yaml` shows the settings).

### Rotating the signing key

A node trusts only the kids built into its binary, and a node reads one
key: a key from a kid it does not know drops it to the free limits.

1. `go run ./hack/licensegen keygen -kid kudurru-<year>-<n> -dir .`; keep
   the `.key` in Infisical, commit the `.pub` to `internal/license/keys/`.
2. Ship a kudurru release with it. Record that version.
3. Rebuild the portal's image, mount the new key, set `LICENSE_SIGNING_KID`
   and `LICENSE_SIGNING_TRUSTED_SINCE` (the version from step 2; pages say
   "Works with kudurru vX or later").
4. Renew or change licenses as they come due; each new key uses the new
   kid. Nodes on older releases need an upgrade first.
5. Drop the old `.pub` in a later release, once no key it signed is in use
   (that also stops every key it signed on upgraded nodes, the only lever
   after a leak).

## Database roles

Two roles, created once by an administrator with
[deploy/postgres-roles.sql](deploy/postgres-roles.sql), before the first
migration:

- `license_portal_owner` owns the schema and is used only by
  `bun dist/server.js migrate` (CI, a workstation or a one-off container);
- `license_portal_app` is the only role the serving container holds.
  Migration `0002` grants it exactly what the server's SQL uses; `0003`
  adds the approval links' table, `0004` deleting users and `0005` the
  business requests' table:

| Table | Runtime privileges |
|---|---|
| `users` | SELECT, INSERT, UPDATE, DELETE |
| `customers`, `licenses`, `license_keys`, `license_requests`, `business_requests` | SELECT, INSERT, UPDATE |
| `sessions`, `customer_members`, `public_key_requests`, `request_approval_links` | SELECT, INSERT, UPDATE, DELETE |
| `oidc_logins`, `blocked_emails` | SELECT, INSERT, DELETE |
| `audit_events` | SELECT, INSERT (and USAGE on `audit_events_id_seq`) |
| `schema_migrations` | SELECT |

It cannot change the schema, truncate anything, delete a license, a key or
a request, or rewrite the audit trail (triggers also refuse UPDATE, DELETE and
TRUNCATE on `audit_events` for every role). `server/test/roles.test.ts`
runs every route as this role and proves what it cannot do.

The simpler single-role setup uses the cloud database's existing credentials
as `DATABASE_URL`; the role must be allowed to create and alter the portal's
schema. No role-creation SQL is needed. The two-role setup above limits the
serving container's database permissions and can be adopted later.

On Cloudflare, GitHub Actions runs migrations automatically before deploying
([cloudflare/README.md](cloudflare/README.md)): `DATABASE_MIGRATE_URL` is
optional and falls back to `DATABASE_URL`. Production runs with
`MIGRATE_ON_START=false`. For a deployment elsewhere, run the same migration
command as a separate release step:

```sh
docker run --rm -e DATABASE_MIGRATE_URL='postgres://license_portal_owner:…@<host>:5432/<db>' \
  ghcr.io/robertherbaugh/kudurru-license-server:<version> migrate
```

The server refuses to start against a schema newer than it knows, so fix
forward rather than rolling back across a migration. Use a direct
connection, not a transaction pooler (PlanetScale: port 5432, not 6432):
the migration lock and Bun's prepared statements need a session.
`DATABASE_TLS=verify-full` (the default) checks the certificate against
Bun's bundled roots or `DATABASE_CA_FILE`; `sslmode=verify-full` in the URL
means the same, and weaker `sslmode` values are refused because Bun's
`sslmode=require` does not verify the certificate.

## Mail

Every portal email includes a branded HTML version and a complete plain text
alternative. The HTML uses Kudurru's Ink, Paper and Clay palette, an embedded
logo, and buttons for confirmation, review and portal access. The logo travels
as an inline CID attachment; no external image or font host is needed. Key
emails still include the exact token and `license.key` attachment. The brand
assets are embedded in the server bundle; regenerate them from `branding`
with `python3 license-server/scripts/mail-brand.py` from the repository root
(Pillow 11.3.0, as for the brand exports).

Mail goes over SMTP (`SMTP_URL`): `smtps://user:pass@host:465` (implicit
TLS) or `smtp://user:pass@host:587` (STARTTLS required). With Cloudflare
Email Service: `smtps://api_token:<API token with Email Sending: Edit>@smtp.mx.cloudflare.net:465`,
with `MAIL_FROM` on a domain onboarded for Email Sending. Without
`SMTP_URL` the public form answers 503 for personal keys and the rest of
the portal works (keys are shown in the portal; "Email key" is
unavailable; no approval mail goes out, so license admins find waiting
requests on the Requests page; business requests are still taken and
listed there, without mail). Set up SPF, DKIM and DMARC for the `MAIL_FROM` domain: the public
flow delivers keys only by mail.

## The kudurru.io form

`POST /api/v1/public/key-requests` with `{"name", "email", "use",
"noncommercial": true, "website": ""}` (the last is a honeypot) from an
origin in `PUBLIC_FORM_ORIGINS`. It answers 202 for every valid request,
400 with `fields`, 429 with `Retry-After` (exposed to the page) or 503. The
portal mails a confirmation link, and the person confirms on `/verify`.
A confirmed request from an address whose personal key is not due for
renewal yet gets that key again by mail (nothing is signed). By default
(`PERSONAL_AUTO_APPROVE=false`) any other confirmed request, a new key or
the renewal of one in its last 30 days, in grace or expired, waits for a
license admin: the page says "Your request is waiting for approval", and
the key goes out by mail with an invite link to the portal once an admin
approves it (below). The Community License eligibility
certification is recorded with the request and then with the license.

## Business requests

The form's business choice posts `{"name", "email", "company", "edition",
"nodes", "details", "website": ""}` to `POST
/api/v1/public/business-requests` from an origin in
`PUBLIC_FORM_ORIGINS`: `edition` is `core` (Commercial Core) or
`enterprise`, `nodes` a whole number from 1 to 10000, `details` optional
(up to 1500 characters), `website` the honeypot. It answers 202 for every
valid request, 400 with `fields` or 429 with `Retry-After`; when the
portal cannot be reached or answers anything else, the form opens an email
to hello@kudurru.io instead. There is no confirmation link (nothing is
signed until a license admin issues a license): the request is stored at
once, the requester gets "We have your kudurru business license request"
with what they sent, and each address of `ADMIN_NOTIFY_EMAIL` (or, when it
is unset, every enabled license admin and superadmin with a verified
email) gets "Business license request from <company>" with a link to the
Requests page. The `PUBLIC_LIMIT_*` limits apply, counted apart from
personal requests.

On the Requests page, the Business tab lists them, open first. A license
admin replies to the requester by email, then either:

- **Issue license**: the issue wizard opens with a new organization
  customer named after the company, the requester's email as its contact,
  the plan of the edition (Commercial Core as `business`, Enterprise as
  `enterprise`) and the nodes asked for, all editable. Signing marks the
  request issued, with a link to the license.
- **Close**: with a note for the record (nothing is mailed).

A request is decided once. The audit trail records
`business_request.received` (without the unconfirmed address),
`business_request.issued` and `business_request.closed`.

## Users

The Users page lists everyone who signed in. A superadmin grants roles,
disables and enables a user, ends their sessions and deletes them. Delete
removes the user, their sessions and their customer memberships; licenses,
keys, requests and the block list keep the name they recorded, and the
audit trail records `user.deleted` with the issuer, subject and email.
Nobody deletes themselves or the last enabled superadmin. Someone who signs
in again after a delete gets a new user without roles; to keep a person
out, disable them instead (and remove them at the identity provider).

## Approving personal keys

With `PERSONAL_AUTO_APPROVE=false` (the default) nothing signs a personal
key on its own. A new key or a renewal from the kudurru.io form, from the
portal's "Request a free personal key", or from the portal's "Request
renewal" on a personal license (with a fresh community-use certification;
the button reads "Renew" when approval is automatic) opens a request on
the Requests page, and the requester gets a mail saying it waits for
approval. A second request while one is open is the same request (and
mails nobody again). Approving renews the person's
active personal key (to 365 days after the later of today and its current
expiry) or issues a new one (personal, 3 nodes, every feature, 365 days)
and mails it to the requester; rejecting needs a note, which is mailed to
the requester.

- **Who is told.** Each address in `ADMIN_NOTIFY_EMAIL` (a comma list), or,
  when it is unset, every enabled license admin and superadmin with a
  verified email. With nobody to tell the request still waits, and the
  server logs `request waiting for review, nobody notified`. The System
  page shows the list under "Approval".
- **The mail.** One per recipient, subject `Approve a free personal key for
  <name>?` (or `Approve the renewal of <license id> for <name>?`): the name,
  email, use and source, the certification (statement, time, IP), why it
  waits, the terms approving signs and, for a renewal, the license it
  would renew with its current expiry. It carries the one-click link and a
  link to the Requests page.
- **The one-click link.** `${PUBLIC_URL}/approve?token=…`: a token per
  recipient that decides once, within 7 days (the constant
  `APPROVAL_LINK_TTL_DAYS` in `shared/catalog.ts`, not a setting). The page
  needs no sign-in. Opening it changes nothing (mail scanners open links):
  it shows the request and the terms, with "Approve and send the key" and
  "Reject". A request decided another way makes its links useless, and a
  link stops working when its address no longer approves (it left
  `ADMIN_NOTIFY_EMAIL` or, when that is unset, its admin was disabled or
  lost the role). The audit trail records the decision with
  `via: approval_link` and the address the link went to, as the one
  enabled admin whose verified sign-in email that is, or else as
  `approval link (<address>)`. Such a system-actor approval does not count
  against `ISSUE_MAX_PER_ACTOR_PER_DAY`.
- **No link for doubtful requests.** A request from a blocked address, or
  from a person with a withdrawn license, gets the mail too, saying why,
  but without the one-click link: a license admin decides on the Requests
  page, signed in.
- **Automatic keys.** `PERSONAL_AUTO_APPROVE=true` signs new keys and
  renewals at once, without a request. Beyond
  `PERSONAL_AUTO_APPROVE_MAX_PER_DAY` new keys a day, requests wait for
  approval as above, with the mail and the link.

## Configuration

Environment variables; every secret also as `<NAME>_FILE` (a file whose
trimmed content is the value). `server/config.ts` `CONFIG_VARS` is the
source of this table (`server/test/readme.test.ts` keeps them in step) and
the System page shows the non-secret part.

| Variable | Default | Meaning |
|---|---|---|
| `PUBLIC_URL` | unset | External URL of the portal: the bare origin (scheme, host, port; no path). Cookie Secure, `__Host-` and HSTS follow its scheme |
| `HOST` | `0.0.0.0` | Listen address |
| `PORT` | `8790` | Listen port |
| `LICENSE_SERVER_DEV` | `false` | Dev mode: test signing keys allowed, a development banner in the UI |
| `DATABASE_URL` | unset | Postgres URL of the runtime role (secret) |
| `DATABASE_MIGRATE_URL` | unset | Postgres URL of the owner role for migrations (secret) |
| `DATABASE_TLS` | `verify-full` | `verify-full` or `disable` |
| `DATABASE_CA_FILE` | unset | CA bundle for verify-full (else Bun's bundled roots) |
| `DATABASE_POOL_MAX` | `10` | Connection pool size |
| `MIGRATE_ON_START` | `true` | Run pending migrations at startup |
| `OIDC_ISSUER` | unset | OIDC issuer URL, exactly as the IdP writes it; https (http only for a loopback issuer while `PUBLIC_URL` is http) |
| `OIDC_CLIENT_ID` | unset | OIDC client id |
| `OIDC_CLIENT_SECRET` | unset | OIDC client secret (secret) |
| `OIDC_SCOPES` | `openid profile email` | Space-separated scopes; add `groups` for IdPs that support it |
| `OIDC_DISPLAY_NAME` | `Sign in` | Sign-in button label |
| `OIDC_GROUPS_CLAIM` | `groups` | The only claim read for groups (string or array) |
| `OIDC_ID_TOKEN_ALG` | unset | Pin the ID token algorithm (asymmetric only) |
| `OIDC_ADMIN_CLAIM_RULES` | unset | `claim=value` comma list; all must match before admin group mappings are honored |
| `ROLE_SUPERADMIN_GROUPS` | empty | IdP groups mapped to superadmin (comma list) |
| `ROLE_LICENSE_ADMIN_GROUPS` | empty | IdP groups mapped to license_admin (comma list) |
| `ROLE_CUSTOMER_GROUPS` | empty | IdP groups mapped to customer (comma list) |
| `DEFAULT_ROLE` | `customer` | `customer` or `none`: the role of a sign-in that maps to nothing |
| `MEMBER_AUTO_LINK` | `false` | Also link member rows by IdP-verified email at sign-in |
| `LICENSE_SIGNING_KEY_FILE` | unset | File with the signing seed (licensegen format) |
| `LICENSE_SIGNING_KID` | unset | Key id of the signing key (`<kid>.pub` in the trusted keys directory) |
| `LICENSE_SIGNING_KEY` | unset | The signing seed itself, for platforms that pass secrets as environment variables only (secret) |
| `LICENSE_TRUSTED_KEYS_DIR` | `internal/license/keys` (image: `/app/trusted-keys`) | Directory of `<kid>.pub` files kudurru releases embed |
| `LICENSE_SIGNING_TRUSTED_SINCE` | unset | First kudurru release that trusts the signing kid, for example `v0.1.0` |
| `REQUIRE_SIGNING_KEY` | `false` | Refuse to start without a usable signing key |
| `SESSION_IDLE_TIMEOUT` | `30m` | Session idle timeout |
| `SESSION_LIFETIME` | `12h` | Absolute session lifetime |
| `TRUSTED_PROXIES` | empty | CIDRs whose `X-Forwarded-For` is believed (comma list; the rightmost untrusted entry is the client) |
| `CLIENT_IP_HEADER` | unset | A header the only proxy in front of the server sets to the client address (`X-Client-IP` behind the Cloudflare Worker) |
| `SMTP_URL` | unset | `smtp://user:pass@host:587` (STARTTLS) or `smtps://…:465` (secret) |
| `MAIL_FROM` | `kudurru license portal <licenses@kudurru.io>` | From header of every mail |
| `MAIL_REPLY_TO` | `hello@kudurru.io` | Reply-To header of every mail |
| `ADMIN_NOTIFY_EMAIL` | unset | Who gets the approval mail of each personal key request, with its one-click link, and the mail of each business request (comma list). Unset: every enabled license admin and superadmin with a verified email |
| `PUBLIC_FORM_ORIGINS` | `https://kudurru.io` | Exact origins of the public form (comma list); `-` disables the public routes |
| `PUBLIC_LIMIT_PER_IP_PER_HOUR` | `5` | Public form requests per IPv4 address or IPv6 /64 per hour (personal and business requests counted apart) |
| `PUBLIC_LIMIT_PER_EMAIL_PER_DAY` | `3` | Public form requests per email per day (silent 202 beyond; personal and business requests counted apart) |
| `PERSONAL_AUTO_APPROVE` | `false` | `true` signs personal keys and renewals without an admin's approval |
| `PERSONAL_AUTO_APPROVE_MAX_PER_DAY` | `50` | With automatic approval: beyond this many automatic personal keys a day, requests wait for approval |
| `ISSUE_MAX_PER_ACTOR_PER_DAY` | `50` | Admin signatures per person per 24 h (429 `issuing_quota`); approvals through a link that names no single admin are not counted |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

The server refuses to start on a configuration it must not run with, with
one sentence naming the variable; it never repeats a secret.

## Deploying

The image is `ghcr.io/robertherbaugh/kudurru-license-server`, published by
`.github/workflows/license-server.yml` on `license-server-v*` tags
(linux/amd64 and linux/arm64, cosign-signed). It runs as uid 65532 on
`oven/bun:1.4.3-distroless`, listens on 8790, answers `GET /healthz` and
`GET /readyz`, and drains on SIGTERM. Put it behind a TLS-terminating proxy
with `PUBLIC_URL=https://…` and `TRUSTED_PROXIES` (or `CLIENT_IP_HEADER`).
One instance is the supported topology; several work, but the sign-in rate
limit counts per instance.

- **Cloudflare Containers**: [cloudflare/README.md](cloudflare/README.md).
- **Anywhere else**: the image with the variables above, secrets as files;
  `compose.yaml` shows the hardening (`read_only`, `cap_drop: [ALL]`,
  `no-new-privileges`, no core dumps).

### Backups and upgrades

Postgres holds who holds which license, the agreement references, the
community-use attestations and the audit trail; keys keep working on the
nodes without it, but the record does not. Take a nightly `pg_dump` (or
the provider's point-in-time recovery) to storage off the host, and test a
restore. The signing key is backed up separately (Infisical) and is never
in the database. An upgrade is: migrate, deploy, check `/readyz`.

## Security notes

- Sign-in is a server-side authorization code flow with PKCE, state and
  nonce bound to a `__Host-` cookie; the ID token signature is verified.
  Sessions live in Postgres (idle 30 minutes, at most 12 hours) and end on
  sign-out, role changes and disabling.
- Every unsafe request needs a same-origin `Sec-Fetch-Site` (or `Origin`)
  and the session's `X-Kudurru-CSRF` token. Only the public form's routes
  are cross-origin, without cookies. The anonymous POSTs of `/verify` and
  `/approve` read no session; the token in the body is the credential.
- A one-click approve link lets whoever holds it decide that one request,
  so point `ADMIN_NOTIFY_EMAIL` only at mailboxes that license admins alone
  read. Each token is 32 random bytes; only its SHA-256 is stored. It
  decides once within 7 days, and opening it changes nothing. Until the
  link is deleted (30 days after it expires) it still shows the request's
  name, email and use to whoever holds it. Its routes are rate limited per
  client address. It never shows the key (the key goes to the requester by
  mail). A blocked address or a withdrawn history never gets a link.
- An approval through a link sent to an address that is not one license
  admin's verified sign-in email is recorded as `approval link (<address>)`
  and does not count against `ISSUE_MAX_PER_ACTOR_PER_DAY`: only signed-in
  admins and the admins a link names have that quota.
- Every mutation is audited in the same transaction; key tokens never reach
  the audit trail or the logs. One log line per signature lets an outside
  log store keep an issuing record the database cannot rewrite.
- Issuing has a per-person daily quota (people only, see above); the public
  form has per-network and per-address limits, a honeypot and email
  confirmation. No personal key is signed before an admin approves it
  (unless `PERSONAL_AUTO_APPROVE=true`).
