# kudurru license portal on Cloudflare Containers

This directory runs the license portal (`license-server/`) on Cloudflare
Containers behind `https://licenses.kudurru.io`. A Worker receives every
request and hands it to one Durable Object, which runs the portal's image as
its container. The server does all the work (sessions, CSRF, CORS, the API
and the UI); the Worker only redirects plain http, writes the client address,
answers a 503 in the server's error shape when the container cannot answer,
and, from a cron trigger, keeps the container warm and restarts it when a
var or secret changed.

Deployment is automatic: after the license portal checks pass, a change on
`main` builds the image, pushes it to Cloudflare, applies pending Postgres
migrations and deploys the Worker with its secrets. Set up the account,
non-secret configuration and GitHub environment once; subsequent releases
need no local Docker commands, manual migrations or image-tag edits.

**No secret values belong in the repository.** `wrangler.jsonc` contains
public settings and secret **names**. Database passwords, the OIDC secret,
the mail token and the private signing seed belong in GitHub environment
secrets, which the deployment uploads to Cloudflare. Public signing keys
(`internal/license/keys/*.pub`) are safe to commit and must ship with the
image; the corresponding private `.key` stays in your secret manager.

No kudurru node ever contacts the portal. Keys are verified offline on each
node (D163); the portal only issues and delivers them.

```mermaid
flowchart LR
    B["browser<br/>(portal, kudurru.io form)"] --> E["Cloudflare edge<br/>TLS, CF-Connecting-IP"]
    E --> W["Worker<br/>src/index.ts"]
    W -->|"X-Client-IP"| DO["Durable Object<br/>LicensePortal"]
    DO --> C["container<br/>bun /app/server.js :8790"]
    C -->|"TLS verify-full, :5432"| PG["PlanetScale Postgres"]
    C -->|"smtps :465"| M["Cloudflare Email Service"]
    C -->|"https"| IDP["OIDC identity provider"]
```

| Path | What it is |
|---|---|
| `wrangler.jsonc` | The Worker, container settings, Durable Object, public vars, cron trigger and required secret names; the image reference is a placeholder for offline checks |
| `scripts/deploy.ts` | Builds and pushes the image, migrates, generates a temporary image reference and deploys with runtime secrets |
| `../../.github/workflows/license-server.yml` | Checks pull requests and deploys changes on `main`; version tags also publish the signed GHCR image |
| `src/index.ts` | The Worker and the `LicensePortal` Durable Object (a `@cloudflare/containers` `Container`) |
| `src/env.ts` | Which vars and secrets reach the container's environment (checked against the server's `CONFIG_VARS`) |
| `src/http.ts` | The request the container gets, and the Worker's own 503 |
| `local/` | `bun run dev`: the same Worker against a locally built image |
| `test/` | `bun test`: environment forwarding, request handling and deployment secret/configuration checks |

## What it costs

Containers need the Workers Paid plan (5 USD a month). The portal runs on
one `basic` instance (1/4 vCPU, 1 GiB memory). Kept warm by the cron trigger
it costs about 7 USD a month on top of the plan; remove the trigger to let it
sleep after 15 quiet minutes (then a request after a pause waits for a cold
start of a few seconds). Cloudflare Email Service includes 3,000 mails a
month on Workers Paid. PlanetScale is billed through Cloudflare.

## One-time setup

You need the Cloudflare account that holds `kudurru.io` on Workers Paid,
a cloud Postgres database, the signing seed from Infisical and an OIDC
client. GitHub Actions supplies Docker and Bun; you do not need them on
your machine to deploy.

1. **Database.** Use your existing cloud database connection, including its
   current/bootstrap password, as the GitHub secret `DATABASE_URL`. That
   role must be able to create and alter the portal's schema. You do not
   need to create separate roles or run migrations yourself.

   For PlanetScale, use the **direct** port 5432, not PgBouncer on 6432:
   transaction pooling breaks the migration lock and Bun's prepared
   statements. Keep `?sslmode=verify-full` in the URL; weaker TLS modes are
   refused. The database must accept connections from both the CI runner
   and Cloudflare Containers.

2. **Mail.** In the dashboard, onboard `kudurru.io` under Email Service,
   Email Sending. Create an account-owned API token with **Email Sending:
   Edit** permission. Put
   `smtps://api_token:<token>@smtp.mx.cloudflare.net:465` in the GitHub
   secret `SMTP_URL`. `MAIL_FROM` in `wrangler.jsonc` must use an address
   on the onboarded domain. Another SMTP provider works too.

3. **Identity provider.** Register a confidential client with redirect URI
   `https://licenses.kudurru.io/api/v1/auth/callback`. Customers and staff
   sign in at the same issuer, so it must let customers register (or be
   invited) and verify email addresses itself (`license-server/README.md`,
   "Who signs in"). Put its client secret in `OIDC_CLIENT_SECRET`.

4. **Public configuration.** Edit and commit `wrangler.jsonc`:
   `OIDC_ISSUER`, `OIDC_CLIENT_ID`, any role mappings,
   `LICENSE_SIGNING_TRUSTED_SINCE` (the first kudurru release that embeds
   the signing kid) and `ADMIN_NOTIFY_EMAIL` (below). Uncomment the
   `routes` entry for `licenses.kudurru.io`. Leave the placeholder `image`
   alone: the deploy script fills it in without modifying the tracked file.
   The script refuses to deploy while the route is disabled or an
   `example.org` placeholder remains.

   `ADMIN_NOTIFY_EMAIL` decides who receives each personal-key request
   and its one-click approval link. Replace its placeholder with a comma
   list of mailboxes only license admins read, or remove the var to mail
   every enabled license admin and superadmin with a verified email. Any
   signed-in license admin can also decide requests on the Requests page.
   `PERSONAL_AUTO_APPROVE=false` remains the default. See
   `license-server/README.md`, "Approving personal keys", for attribution
   and quotas when approving by mail.

5. **GitHub environment.** In the repository's Settings → Environments,
   create `license-portal-production`. Allow deployments from `main`.
   Leave required reviewers unset for automatic deployment; add a reviewer
   only if you want an approval gate.

   Add this **environment variable**:

   | Name | Value |
   |---|---|
   | `CLOUDFLARE_ACCOUNT_ID` | The account ID from the Cloudflare dashboard (not a zone ID) |

   Add these **environment secrets**, never to `wrangler.jsonc`:

   | Name | Value |
   |---|---|
   | `CLOUDFLARE_API_TOKEN` | A Cloudflare deployment token for this account and zone, allowed to deploy Workers and Containers, push to the managed container registry, and manage the Worker's custom domain |
   | `DATABASE_URL` | Your direct cloud Postgres URL, including the existing password and `?sslmode=verify-full` |
   | `OIDC_CLIENT_SECRET` | The identity provider's client secret |
   | `LICENSE_SIGNING_KEY` | The real licensegen seed from Infisical: standard base64 of 32 bytes, matching `LICENSE_SIGNING_KID` and its committed `.pub` |
   | `SMTP_URL` | The SMTP URL from step 2, including its token/password |

   For the deployment token, start with Cloudflare's **Edit Cloudflare
   Workers** API token template and add **Account → Containers → Edit**
   for this account. Keep the Workers and zone permissions needed to
   deploy the custom domain, scoped to `kudurru.io`. The SMTP token is
   separate and needs only Email Sending permission. See
   [Cloudflare's token permissions](https://developers.cloudflare.com/fundamentals/api/reference/permissions/).

   If you already configured the Worker with `wrangler secret put`, copy
   those same values into the GitHub environment: CI is now their source.
   The deployment token is used only by CI; it never reaches the portal.
   Cloudflare receives runtime secrets through Wrangler's `--secrets-file`
   in the same Worker deployment, including the very first deployment.
   The temporary file has mode `0600`, lives under gitignored `.wrangler/`
   and is deleted even if a deployment command fails. Secrets never enter
   Docker build arguments or the image.

6. **Deploy.** Merge the configuration and workflow to `main`, or run
   **Actions → license-server → Run workflow** on `main` after configuring
   the environment. The existing checks must pass first. The first deploy
   creates the custom domain's DNS record and certificate; remove any
   conflicting DNS record beforehand. Check `https://licenses.kudurru.io`.

7. **The first superadmin.** Prefer mapping your identity provider's admin
   group with `ROLE_SUPERADMIN_GROUPS`; members get the role when they sign
   in, with no database command. If your provider has no group mapping,
   sign in once and use the existing one-time `grant-superadmin` command
   from `license-server/README.md`. It is also the recovery path when the
   last superadmin is gone. The portal uses OIDC sign-in, not a separate
   bootstrap login password.

### Optional: separate database roles

When you want narrower runtime permissions, create `license_portal_owner`
and `license_portal_app` with `license-server/deploy/postgres-roles.sql`,
following `license-server/README.md`, "Database roles". On an already
migrated database, transfer ownership of the portal's schema objects to
`license_portal_owner` and apply the grants shown in the SQL file (past
migrations are not rerun). Set the owner's connection as GitHub secret
`DATABASE_MIGRATE_URL`, and change `DATABASE_URL` to the app connection.
The deployment uses the optional owner URL only for migrations; it never
uploads it to Cloudflare or passes it into the serving container.

Until then, omitting `DATABASE_MIGRATE_URL` uses `DATABASE_URL` for both
migrations and serving. The existing/bootstrap database password works;
using it means the serving container also has that role's schema privileges.

## Releasing

A change affecting the license portal on `main` runs the existing checks
and then the `deploy` job in `.github/workflows/license-server.yml`:

1. Build `license-server/Dockerfile` for `linux/amd64`, the architecture
   Cloudflare runs, with the commit and build metadata.
2. Push the image to the account's managed Cloudflare registry, with a
   unique commit/run/attempt tag. A retry also gets a new tag so a deploy
   starts a rollout.
3. Run the **new image's** `migrate` command against the cloud database,
   using `DATABASE_MIGRATE_URL` when supplied, otherwise `DATABASE_URL`.
   The migration runner locks the database and applies only pending
   migrations. A failure stops the release before the Worker is deployed.
4. Generate a temporary Wrangler config with that image reference and
   deploy the Worker, container and runtime secrets together.
5. Retry `/readyz` while provisioning completes, checking the database,
   schema and signing key. This checks readiness at the public route;
   it does not prove every instance has completed its rollout.

Production deployments are serialized and never canceled midway by a
newer run. Pull requests and non-`main` manual runs have no production
deployment. `license-server-v<version>` tags still publish and sign the
multi-architecture GHCR image; they do not cause a second Cloudflare deploy.

The image is built explicitly because Wrangler pipes Dockerfiles to
`docker build -f -`, bypassing `license-server/Dockerfile.dockerignore`.
That allowlist keeps secrets out of the build context; the root
`.dockerignore` excludes `license-server/`. CI handles this detail, so
there are no manual image pushes or edits to `wrangler.jsonc` per release.

Cloudflare still needs the Worker and Durable Object to route requests to
one container; Postgres remains your managed cloud database. Migrations
are the portal's schema updates, separate from Cloudflare's Durable Object
storage configuration. The docs site uses Cloudflare Workers Builds to
deploy from GitHub, while its GitHub Actions workflow only checks it.
[Workers Builds also supports Containers](https://developers.cloudflare.com/containers/guides/deploy/#deploy-with-workers-builds);
this workflow keeps the existing checks, Docker allowlist and database
migration step in one release pipeline.

The rollout replaces the one instance in one step. The server drains for
up to 20 seconds on SIGTERM; requests during replacement may receive the
Worker's 503 with `Retry-After`. Migrations must stay compatible with the
previous release while it drains. The server refuses to start against a
schema newer than it knows, so fix forward rather than rolling back across
a migration. If the image upload succeeds but migration or deployment
fails, the pushed image remains in the registry and CI reports failure;
rerun the workflow after fixing the cause.

For a local release, install the pinned Bun version and Docker, export the
same variables/secrets into your shell, then run:

```sh
cd license-server/cloudflare
bun install --frozen-lockfile
bun run deploy
```

## Changing configuration

Commit changes to public `vars` in `wrangler.jsonc`; the normal `main`
workflow deploys them. Change a secret in the GitHub environment, then run
**Actions → license-server → Run workflow** on `main`. Changing a GitHub
secret alone does not trigger a workflow.

A running process keeps its startup environment. After the Worker deploys,
the next cron tick (within 5 minutes) notices changed vars or secrets and
restarts the container. Direct dashboard or `wrangler secret put` changes
are replaced by the values from GitHub on the next CI deployment.

## Logs and checks

`observability` is on: Worker, Durable Object and container logs (the
server's JSON lines) are in the dashboard for 7 days, and `bunx wrangler
tail` follows the Worker. `GET /readyz` reports the database, migrations
and signing (`"signing":"ready"` once the key loaded).

`bun run check` type-checks the Worker, runs the tests and does `wrangler
deploy --dry-run` (no Docker, no login). `bun run dev` runs the Worker and
the locally built image (`make license-server-image`) with `wrangler dev`;
it needs Docker Desktop, Colima or another host where Cloudflare's local
proxy sidecar works (on Linux kernel 6.18 it fails,
cloudflare/containers#231). Develop the portal itself with
`license-server/compose.yaml`.

## Check on the first real deploy

These could not be verified without an account:

1. The headers the container sees (Cookie, Origin, Sec-Fetch-Site, the
   X-Client-IP the Worker writes): sign in, then look at an audit event's IP.
2. Outbound connections to PlanetScale on 5432 and to SMTP on 465.
3. That `max_instances: 2` gives a rollout room for the replacement.
4. That zone security features (Bot Fight Mode, WAF rules) do not challenge
   the cross-origin POST from the kudurru.io form.
5. Memory and CPU headroom on `basic`.
