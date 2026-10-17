# The kudurru docs site

Anyone may read and link to the published documentation without a purchase
or login. The source and build system have a [separate license notice](LICENSE.md):
community previews and contributions follow the root community terms, and
commercial internal mirrors require an accepted agreement and Order Form.
The website in `website/` has separate source-viewing terms.

A [Hugo](https://gohugo.io) site that publishes the markdown under `docs/`
and a few root files without moving or rewriting them, plus pages of its
own: the landing page, one page per role, migration guides and the
generated references.

```sh
make docs-serve        # http://localhost:1313/, rebuilds on change (PORT=... to move it)
make docs-site         # docs-site/public, fails on any unresolved link
make docs-check-links  # the same build for the root of a domain, checked with htmltest
make docs-check-workers # the build Cloudflare runs, and `wrangler deploy --dry-run`
make docs-gen          # regenerate docs-site/content/reference after a schema, CRD, CLI or API change
```

`make` downloads the pinned Hugo extended release (`HUGO_VERSION` in the
Makefile) into `bin/` and verifies its SHA-256; elsewhere than linux
amd64/arm64 pass `HUGO=/path/to/hugo`.
`make docs-check-workers` also uses Bun to install Wrangler from
`docs-site/package.json` and `docs-site/bun.lock`; building the Hugo site alone does
not require Bun.

## Where the pages come from

| Site section | Source |
|---|---|
| Home, Get started, Migrate, section pages | `docs-site/content` |
| Learn | `docs/guide`: short task pages for first use, filtering, DHCP and local DNS; the administrator reference keeps the full chapters |
| Deploy | `docs/deploy` (its `README.md` is the section page) |
| Enterprise | `docs/enterprise.md`: current features, proposed pricing and node counting |
| Reference | `docs-site/content/reference`, generated: `hack/docsgen` (settings, CRDs, endpoint index) and `TestCLIReferenceDrift` in `cmd/kudurru` (CLI) |
| Reference → API explorer | `api/openapi.json`, rendered by the Scalar bundle in `internal/api/scalar` |
| Architecture | `docs/architecture` (its `README.md` is the section page); the hand-drawn diagrams are served from `docs/architecture/diagrams`, and the Mermaid blocks are drawn in the browser by the bundle in `docs-site/vendor/mermaid`, loaded only on pages that have one |
| Project | `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md`, `LICENSE`, `LICENSING.md`, `COMMERCIAL_LICENSE_AGREEMENT.md` and the folder-specific license notices |

`dev-docs/` is for contributors and stays in the repository: the design
record and decision log (`dev-docs/design-record.md`,
`dev-docs/open-questions.md`) and the engineering notes
(`dev-docs/milestones/m1` ... `dev-docs/milestones/m5`, `dev-docs/spikes`,
`dev-docs/research`, `dev-docs/followups.md`, `dev-docs/release-checklist.md`).
A page of the site that links to one of them gets a GitHub URL (below).

The mounts are in `hugo.yaml`. Pages from `docs/` need no front matter: the
title is the first `#` heading, and the order within a section comes from the
`cascade` list in `hugo.yaml`. `branding` supplies the palette
(`tokens.css`), the logos and the favicon.

## Links

Write links in `docs/` the way GitHub resolves them, relative to the file:
`[runbooks](runbooks.md#upgrading)`, `[architecture](../architecture/README.md)`. The link
hook (`layouts/_markup/render-link.html`) turns a link to a file that is a
page into the page's URL, a link to a directory into its `README.md` page,
and a link to any other repository file into a GitHub URL at the ref being
built. Pages in `docs-site/content` may also use site paths (`/deploy/helm/`).
A link to a missing page, file or anchor fails the build.

## Theme

The theme lives in `layouts/` and `assets/`: plain HTML templates, one
stylesheet and one small script (menu, copy buttons, search over
`search-index.json`). Docsy was not used: it needs Hugo modules fetched from
GitHub at build time (or a vendored tree of Bootstrap and Font Awesome) and
an npm toolchain (PostCSS, autoprefixer) for its stylesheets, while this
site builds offline with the Hugo binary alone.

## Publishing

The site is **https://docs.kudurru.io**, served by Cloudflare Workers as
static assets: no Worker script, so no request counts as a Worker
invocation. Cloudflare Workers Builds builds and deploys it from GitHub;
no workflow in this repository holds Cloudflare credentials.
`.github/workflows/docs.yml` builds and checks the site on every pull request
(generated references, links, the HTML, and `wrangler deploy --dry-run` on
`wrangler.jsonc`).

| File | What it does |
|---|---|
| `wrangler.jsonc` | The Worker `kudurru-docs`: assets from `public/`, `404.html` for unknown paths, `/guide` redirects to `/guide/`, the custom domain `docs.kudurru.io`, no `workers.dev` address |
| `package.json`, `bun.lock` | The pinned Wrangler dependency used for local checks and Cloudflare builds |
| `static/_headers` | Response headers: a content security policy (own files only; inline styles for the API explorer), HSTS, `nosniff`, no framing, long caching for the fingerprinted CSS and JS |
| `hugo.yaml` | Mounts `docs/guide/images` at `/guide/images`, so the guide's screenshots are files of the site |
| `layouts/_markup/render-image.html` | Serves those images from the site's own origin. An image anywhere else in the repository is a build warning: GitHub's raw host is not served to visitors while the repository is private, and the content security policy does not allow it |

### Walkthroughs and screenshots

The First steps and DHCP sections use `walkthrough: true` in `hugo.yaml`.
Page weights set the order of the step indicator and Back/Next links.
Guide pages stay plain Markdown with an H1 title; subsection `README.md` files
have explicit mounts to `_index.md`. Add a matching mount and exclusion when
adding a new subsection.

Keep task pages focused on one outcome, with prerequisites, numbered steps,
screenshots beside the relevant steps and a result check. Keep advanced
options in linked reference pages. Preserve published heading anchors when
splitting a page. The old `/deploy/dhcp/` anchors link to the smaller reference pages.

Screenshots open at full size when clicked. Capture them from the dashboard's
mock mode with `dashboard/scripts/guide-screenshots.mjs`; see the
[guide writing rules](../docs/guide/style.md#take-the-screenshots-again).
The DHCP groups cover all four wizard steps, staged activation, running
leases and a reservation dialog. No live DHCP service is needed for these images.

### One-time setup in the Cloudflare dashboard

The website (`website/`, kudurru.io) is a separate Workers Builds project; this is
the second one. `kudurru.io` must be a zone on the same Cloudflare account.

1. **Connect the repository.** Workers & Pages → Create → Import a repository
   → `robertherbaugh/kudurru`, with these build settings:

   | Setting | Value |
   |---|---|
   | Project name | `kudurru-docs` (must match `name` in `wrangler.jsonc`) |
   | Production branch | `release`, so the site always shows the released version |
   | Root directory | `docs-site` |
   | Build command | `bun install --frozen-lockfile && make -C .. docs-site HUGO_FLAGS="--minify --gc --cleanDestinationDir --panicOnWarning --baseURL https://docs.kudurru.io/"` |
   | Deploy command | `bunx wrangler deploy` |
   | Preview command | `bunx wrangler preview` (a preview URL per branch and pull request) |
   | Build watch paths, include | `docs-site/*`, `docs/*`, `branding/*`, `api/openapi.json`, `internal/api/scalar/*`, `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md` |
   | Build variables | `HUGO_PARAMS_REPOREF=release`, `BUN_VERSION=1.4.3`, `SKIP_DEPENDENCY_INSTALL=1` (the build command installs from `bun.lock`) |

   A build of any other branch, including `main` when production is `release`,
   runs the Preview command against that branch's commit. The command does
   not choose which branch to clone.

   **Existing projects:** if Settings → Builds shows **Set up Worker
   Previews**, complete that setup before using the Preview command. This
   is a one-way switch from the older version URL preview system; follow
   [Cloudflare's migration steps](https://developers.cloudflare.com/workers/ci-cd/builds/build-branches/#existing-workers-connected-to-builds).
   `wrangler.jsonc` includes the required `previews` block. This static site
   needs no separate preview bindings; assets and compatibility settings
   stay at the top level.

   The build reads `docs/`, `branding/`, `api/` and the root markdown files outside the
   root directory; Workers Builds clones the whole repository. `make` downloads
   the pinned Hugo release into `bin/` and checks its SHA-256 (it needs `curl`,
   `tar`, `gunzip` and `git`, all in the build image). `HUGO_PARAMS_REPOREF`
   is the ref that the site's links to repository files point at.

2. **Domain.** The first deploy attaches `docs.kudurru.io` as the Worker's
   custom domain (`routes` in `wrangler.jsonc`) and creates its DNS record and
   certificate. Remove any earlier `CNAME docs` record that points at GitHub
   Pages first, and leave GitHub Pages turned off for this repository. The
   website's Bulk Redirect list (`website/README.md`) does not cover this name.

3. **Check it.** Open https://docs.kudurru.io/guide/user-guide/: the
   screenshots load. Open `/reference/api/explorer/`: the API reference
   renders. Open a path that does not exist: the 404 page answers with status 404.

Links to repository files (the "Source" line and files that are not site
pages) use the configured repository ref. A private repository requires
GitHub sign-in for those links. The site itself, including every image,
does not depend on GitHub access.

The Hugo build writes to `docs-site/public/`, which is not committed.

If GitHub reports **Preview creation failed** and Cloudflare has no build
log, preview provisioning may have failed before the build command started.
Changing the GitHub Actions runner or adding flags to the deploy command
cannot repair that stage. Contact Cloudflare with the Worker name, failing
build ID and GitHub check URL so they can inspect its preview build state.
