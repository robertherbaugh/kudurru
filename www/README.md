# kudurru.io

The project website: one page that shows what kudurru does and sends
readers on to the documentation at [docs.kudurru.io](https://docs.kudurru.io)
(the Hugo site in [`site/`](../site/README.md)) and to the repository. It is
built with [Bun](https://bun.sh) and TypeScript and served by Cloudflare
Workers as static assets: there is no Worker script and no server code.

```sh
make www-dev      # http://localhost:8787/ through `wrangler dev`, rebuilds on change
make www-check    # typecheck, tests, build into www/dist, wrangler dry run
```

Or inside `www/`: `bun install`, `bun run dev`, `bun test`, `bun run build`,
`bun run check:deploy`.

## Layout

| Path | What it is |
|---|---|
| `src/content.ts` | Every claim, number and link on the page. Claims follow `CHANGELOG.md`, the README and `bench/baseline.txt`; change them here when a feature ships or moves |
| `src/pages/` | The page templates: plain functions returning HTML through the escaping `html` tag in `src/html.ts`, rendered once at build time |
| `src/client/` | The only script: tabs, the sample query log in the hero, copy buttons, the mobile menu. The page is complete without it |
| `src/styles/site.css` | The stylesheet. It imports `docs/brand/tokens.css` |
| `src/headers.ts` | The response headers (CSP, HSTS, caching), written to `dist/_headers` |
| `build.ts` | Renders the pages, bundles the script and stylesheet with hashed names, copies the fonts, brand artwork and screenshots, writes `_headers`, `robots.txt`, `sitemap.xml` and the web manifest |
| `wrangler.jsonc` | The Cloudflare Workers configuration: static assets from `dist/`, the 404 page, the `kudurru.io` custom domain |
| `test/` | `bun test`: escaping, `_headers`, and the built pages (no inline script or style, links, anchors, ARIA references, image attributes) |

The page uses the brand kit in [`docs/brand`](../docs/brand/README.md): the
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

## How Cloudflare serves it

- Every file in `dist/` is a static asset. Requests never run a Worker, so
  they do not count against Worker request limits.
- `dist/_headers` gives every response the Content-Security-Policy (no
  inline script or style; the build tests enforce it), HSTS and the
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
   | Production branch | `release`, so the site always shows the released version. On `main`, `VERSION` already names the next, unpublished release |
   | Root directory | `www` |
   | Build command | `bun install --frozen-lockfile && bun run build` |
   | Deploy command | `bunx wrangler deploy` |
   | Preview command | `bunx wrangler preview` (a preview URL per branch and pull request) |
   | Build watch paths, include | `www/*`, `docs/brand/*`, `docs/guide/images/*`, `VERSION` |
   | Build variables | `BUN_VERSION=1.4.3`, `SKIP_DEPENDENCY_INSTALL=1` (the build command installs from `bun.lock`) |

   The build reads `../VERSION`, `../docs/brand` and `../docs/guide/images`
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
   its own custom domain (root directory `site`); set it up from
   [site/README.md](../site/README.md#publishing). The Bulk Redirect list
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

[`www.yml`](../.github/workflows/www.yml) runs `make www-check` on every
pull request that touches the site, the brand kit or the screenshots. It
holds no Cloudflare credentials; only Workers Builds deploys.
