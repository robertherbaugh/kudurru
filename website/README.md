# kudurru.io

Copyright 2026 kudurru project maintainers. The project-controlled source is
available for inspection under the [Kudurru Website Source-Viewing License](LICENSE.md).
Modification, redistribution and hosting require prior written authorization.
A Kudurru community or commercial license does not grant those website rights.
The build and deployment instructions below are for authorized maintainers.

The project website provides an overview, feature comparisons and use-case
pages. It links to the documentation at [docs.kudurru.io](https://docs.kudurru.io)
(the Hugo site in [`docs-site/`](../docs-site/README.md)) and to the repository. It is
built with [Bun](https://bun.sh) and TypeScript and served by Cloudflare
Workers as static assets: there is no Worker script and no server code.

```sh
make website-dev      # http://localhost:8787/ through `wrangler dev`, rebuilds on change
make website-check    # typecheck, tests, build into website/dist, wrangler dry run
```

Or inside `website/`: `bun install`, `bun run dev`, `bun test`, `bun run build`,
`bun run check:deploy`.

## Layout

| Path | What it is |
|---|---|
| `src/content.ts` | Every claim, number and link on the page. Claims follow `CHANGELOG.md`, `docs/architecture`, `docs/deploy` and `bench/baseline.txt`; change them here when a feature ships or moves |
| `src/use-cases.ts` | The Kubernetes DNS, network ad blocking and Pi-hole alternative landing pages, with links to maintained setup and migration guides |
| `src/seo.ts` | Home search title and JSON-LD for the site, publisher, software and page breadcrumbs |
| `src/pages/` | The page templates: plain functions returning HTML through the escaping `html` tag in `src/html.ts`, rendered once at build time |
| `src/client/` | The only script: tabs, the sample query log in the hero, copy buttons, the mobile menu, the enterprise pricing calculator, and the "Request a key" form of `/license/` (`license.ts`). Pages are complete without it; the calculator shows a static example and the key form shows mail links instead |
| `src/styles/site.css` | The stylesheet. It imports `branding/tokens.css` |
| `src/headers.ts` | The response headers (CSP, HSTS, caching), written to `dist/_headers` |
| `build.ts` | Renders the pages and root Community License, bundles the script and stylesheet with hashed names, copies the fonts, brand artwork and screenshots, writes `_headers`, `robots.txt`, `sitemap.xml` and the web manifest |
| `wrangler.jsonc` | The Cloudflare Workers configuration: static assets from `dist/`, the 404 page, the `kudurru.io` custom domain |
| `test/` | `bun test`: escaping, `_headers`, and the built pages (no inline executable script or style, links, anchors, ARIA references, image attributes, metadata, sitemap and JSON-LD) |

The page uses the brand kit in [`branding`](../branding/README.md): the
Ink, Clay, Paper and Stone tokens, the supplied logo and mark PNGs unchanged,
Inter and JetBrains Mono (self-hosted from `@fontsource-variable`, OFL-1.1,
licenses served at `/licenses/`). Query-status colors in the sample log are
separate from the brand colors, as the kit asks. The dashboard screenshots
are the guide's (`docs/guide/images`), so they stay current with it.

The sample traffic in the hero and the pipeline section is illustrative and
says so on the page: names under `.example` and addresses from RFC 5737 and
RFC 3849 are documentation placeholders.

The version on the page (the badge and the `docker run` and Helm commands)
comes from the repository's `VERSION` file; `WWW_VERSION` overrides it.

## Homelab

The Homelab page keeps its existing `/move-over/` URL. It covers local DNS,
household filtering, Docker and Kubernetes deployment, and free community
keys, followed by the Pi-hole, AdGuard Home, Technitium and kudurru v0.1.0
comparison. The home page and footer link to all three migration guides at
`docs.kudurru.io/migrate/`. Comparison claims and source links live in
`src/content.ts`; the page template is `src/pages/move-over.ts`. Keep the
comparison and limitations in step with `docs-site/content/migrate/`, especially
when deferred features ship. The docs own the configuration examples,
validation steps, cutover and rollback procedures.

## Search and discovery

The home page and every indexable page have unique titles and descriptions,
production canonical URLs, Open Graph and Twitter cards, and JSON-LD.
The software schema follows `VERSION` and links to the real license; it
does not publish prices, reviews or ratings. Exact SHA-256 hashes authorize
the JSON-LD blocks in the CSP. Executable scripts remain external, and
`unsafe-inline` is never enabled.

`build.ts` collects indexable paths when it renders each page, then writes
the sitemap from that collection. A new page rendered through `writePage`
is included automatically. The 404 page is excluded. `robots.txt` permits
crawling and advertises `https://kudurru.io/sitemap.xml`.

The landing pages `/kubernetes-dns/`, `/network-ad-blocking/` and
`/pi-hole-alternative/` answer different deployment questions and link to
the corresponding docs. They are linked from the home page, every footer
and each other. Their current capabilities and limitations follow
`docs-site/content/start/`, `docs-site/content/migrate/` and the deployment guides;
keep them current when those change.

For a preview build, set `WWW_NOINDEX=1` (or call `build` with
`noIndex: true`). This emits a `noindex` meta tag on every page and an
`X-Robots-Tag: noindex` response header, omits structured data and canonical
links, and produces an empty sitemap. Crawling remains allowed so search
engines can read the directive. Set it only on non-production builds.

The account setup, domain checks and ongoing measurement are in the
[SEO launch guide](SEO.md). Those steps require access to the domain and
search engine accounts; a site build does not submit a sitemap or verify
ownership.

## Enterprise

The `/enterprise/` page describes current enterprise capabilities, the
Commercial Core and Enterprise scopes, node counting and annual USD pricing.
Commercial Core is $99/year plus $99/node/year; Enterprise is $499/year plus
$299/node/year. `src/pricing.ts` shares the calculation between the static
examples and browser calculator: 25–49 nodes receive 10% off node fees,
and 50+ nodes need a quote. Support is separate. Without JavaScript, the
calculator shows the five-node Enterprise example and hides its controls.
Keep `src/content.ts`, `src/pricing.ts` and `src/pages/enterprise.ts` in
step with the documentation source, [`docs/enterprise.md`](../docs/enterprise.md),
published at `docs.kudurru.io/enterprise/`. Enterprise inquiries open email
to `hello@kudurru.io` without submitting a form.

## The "Request a key" form

`/license/` has a form beside the introductory text (stacked below it on
narrow screens), with two kinds of request. A **personal** request is
sent from the visitor's browser to the license server
(`POST <origin>/api/v1/public/key-requests` with `name`, `email`, `use` and an
empty honeypot `website`; the server emails a confirmation link; after the
visitor confirms, an administrator approves the request and the server emails
the key; a `202` is the answer to any valid request). A **business or other**
request goes to the license server too (`POST
<origin>/api/v1/public/business-requests` with `name`, `email`, `company`,
`edition` (`core` or `enterprise`), `nodes`, `details` and the honeypot
`website`): the server stores it for the license admins (the portal's
Requests page, Business), emails the visitor that it arrived and replies by
email later; a `202` is the answer to any valid request. When the server
cannot be reached or answers `503` (or another unexpected status), the
browser opens a mail to `hello@kudurru.io` with the subject `Business
License` and every field in the body instead, and shows the same text to
copy if no mail program opens.

The license server's origin is `https://licenses.kudurru.io` unless
`WWW_LICENSE_API` names another one at build time (set it in the Cloudflare
build variables). Three things must agree with it:

- the site's CSP allows exactly that origin in `connect-src` (`src/headers.ts`
  writes it into `_headers`; the build fails on a malformed URL);
- the server's CORS list (`PUBLIC_FORM_ORIGINS`) contains `https://kudurru.io`
  and exposes `Retry-After`;
- the server's limits are the form's (name 200, email 254, use 1000,
  business 160 and details 1500 characters, 1 to 10000 nodes), kept in
  `src/client/license.ts`.

## How Cloudflare serves it

- Every file in `dist/` is a static asset. Requests never run a Worker, so
  they do not count against Worker request limits.
- `dist/_headers` gives every response the Content-Security-Policy (no
  inline executable script or style; JSON-LD uses exact hashes), HSTS and the
  hardening headers, and files under `/assets/` (hashed names) a one-year
  immutable cache. Cloudflare compresses responses itself.
- `not_found_handling: "404-page"` answers unknown paths with `404.html`
  and status 404; `html_handling: "auto-trailing-slash"` redirects
  `/licenses` to `/licenses/`.
- The alias domains are redirected by Cloudflare, not by this project
  (step 3 below).

## Deploying

Cloudflare Workers Builds deploys from GitHub. One-time setup in the
Cloudflare dashboard:

1. **Connect the repository.** Workers & Pages → Create → Import a
   repository → `robertherbaugh/kudurru`, with these build settings:

   | Setting | Value |
   |---|---|
   | Project name | `kudurru-www` (must match `name` in `wrangler.jsonc`) |
   | Production branch | `release`, so the site shows the released version. After each release, bump `VERSION` on `main` for the next development cycle |
   | Root directory | `website` |
   | Build command | `bun install --frozen-lockfile && bun run build` |
   | Deploy command | `bunx wrangler deploy` |
   | Preview command | `bunx wrangler preview` (a preview URL per branch and pull request) |
   | Build watch paths, include | `website/*`, `branding/*`, `docs/guide/images/*`, `VERSION`, `LICENSE`, `LICENSING.md`, `COMMERCIAL_LICENSE_AGREEMENT.md` |
   | Build variables | `BUN_VERSION=1.4.3`, `SKIP_DEPENDENCY_INSTALL=1` (the build command installs from `bun.lock`) |

   The build reads `../VERSION`, `../branding` and `../docs/guide/images`
   outside the root directory; Workers Builds clones the whole repository.
   Each build uses the commit from the branch that triggered it. With
   `release` as production, `main` and feature branches run the Preview
   command; the command does not choose which branch to clone.

   **Existing projects:** if Settings → Builds shows **Set up Worker
   Previews**, complete that setup before using the Preview command. This
   is a one-way switch from the older version URL preview system; follow
   [Cloudflare's migration steps](https://developers.cloudflare.com/workers/ci-cd/builds/build-branches/#existing-workers-connected-to-builds).
   `wrangler.jsonc` includes the required `previews` block. This static site
   needs no separate preview bindings; assets and compatibility settings
   stay at the top level.

2. **Main domain.** `kudurru.io` must be a zone on the same Cloudflare
   account. The first deploy attaches it as the Worker's custom domain
   (`routes` in `wrangler.jsonc`) and creates its DNS record and
   certificate.

3. **Alias domains.** `www.kudurru.io`, `kudurru.app` and `kudurru.dev`
   (each with `www`) redirect to `https://kudurru.io` with a Bulk Redirect,
   which runs before any Worker and costs nothing:
   - In each zone (`kudurru.io` for `www`, `kudurru.app`, `kudurru.dev`),
     add proxied DNS records for the redirected names, for example
     `AAAA @ 100::` and `AAAA www 100::` (orange cloud). They have no
     origin; the redirect answers first.
   - Account → Bulk Redirects → create a list `kudurru-aliases`:

     | Source URL | Target URL | Status | Options |
     |---|---|---|---|
     | `kudurru.app` | `https://kudurru.io` | 301 | include subdomains, subpath matching, preserve path suffix, preserve query string |
     | `kudurru.dev` | `https://kudurru.io` | 301 | the same |
     | `www.kudurru.io` | `https://kudurru.io` | 301 | subpath matching, preserve path suffix, preserve query string |

   - Create a Bulk Redirect Rule that enables the list.

   `.app` and `.dev` are HSTS-preloaded top-level domains, so browsers only
   reach them over HTTPS; Cloudflare's Universal SSL certificate covers the
   apex and `www`.

4. **Docs domain.** `docs.kudurru.io` is not part of this Worker. It is a
   second Worker, `kudurru-docs`, with its own Workers Builds project and
   its own custom domain (root directory `docs-site`); set it up from
   [docs-site/README.md](../docs-site/README.md#publishing). The Bulk Redirect list
   above does not cover it. Do not add a DNS record for `docs` by hand:
   the first deploy of that Worker creates it.

Worker Previews have their own URLs on `workers.dev`; production stays on
`kudurru.io`. The Worker's production `workers.dev` address stays disabled.
`preview_urls` also leaves version URLs available, but branch builds use
`wrangler preview` rather than `wrangler versions upload`.

If GitHub reports **Preview creation failed** and Cloudflare has no build
log, preview provisioning may have failed before the build command started.
Changing the GitHub Actions runner or adding flags to the deploy command
cannot repair that stage. Contact Cloudflare with the Worker name, failing
build ID and GitHub check URL so they can inspect its preview build state.

[`website.yml`](../.github/workflows/website.yml) runs `make website-check` on every
pull request that touches the site, the brand kit or the screenshots. It
holds no Cloudflare credentials; only Workers Builds deploys.
