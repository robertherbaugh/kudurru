# The kudurru docs site

A [Hugo](https://gohugo.io) site that publishes the markdown under `docs/`
and a few root files without moving or rewriting them, plus pages of its
own: the landing page, one page per role, migration guides and the
generated references.

```sh
make docs-serve        # http://localhost:1313/, rebuilds on change (PORT=... to move it)
make docs-site         # site/public, fails on any unresolved link
make docs-check-links  # the same build for the root of a domain, checked with htmltest
make docs-check-workers # the build Cloudflare runs, and `wrangler deploy --dry-run`
make docs-gen          # regenerate site/content/reference after a schema, CRD, CLI or API change
```

`make` downloads the pinned Hugo extended release (`HUGO_VERSION` in the
Makefile) into `bin/` and verifies its SHA-256; elsewhere than linux
amd64/arm64 pass `HUGO=/path/to/hugo`.

## Where the pages come from

| Site section | Source |
|---|---|
| Home, Get started, Migrate, section pages | `site/content` |
| Deploy | `docs/deploy` (its `README.md` is the section page) |
| Reference | `site/content/reference`, generated: `hack/docsgen` (settings, CRDs, endpoint index) and `TestCLIReferenceDrift` in `cmd/kudurru` (CLI) |
| Reference → API explorer | `api/openapi.json`, rendered by the Scalar bundle in `internal/api/scalar` |
| Design | `docs/architecture.md`, `docs/open-questions.md` |
| Engineering notes | `docs/m1` ... `docs/m5`, `docs/spikes`, `docs/research`, `docs/followups.md`, `docs/release-checklist.md` |
| Project | `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md` |

The mounts are in `hugo.yaml`. Pages from `docs/` need no front matter: the
title is the first `#` heading, and the order within a section comes from the
`cascade` list in `hugo.yaml`. `docs/brand` supplies the palette
(`tokens.css`), the logos and the favicon.

## Links

Write links in `docs/` the way GitHub resolves them, relative to the file:
`[runbooks](runbooks.md#upgrading)`, `[design](../architecture.md)`. The link
hook (`layouts/_markup/render-link.html`) turns a link to a file that is a
page into the page's URL, a link to a directory into its `README.md` page,
and a link to any other repository file into a GitHub URL at the ref being
built. Pages in `site/content` may also use site paths (`/deploy/helm/`).
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
| `static/_headers` | Response headers: a content security policy (own files only; inline styles for the API explorer), HSTS, `nosniff`, no framing, long caching for the fingerprinted CSS and JS |
| `hugo.yaml` | Mounts `docs/guide/images` at `/guide/images`, so the guide's screenshots are files of the site |
| `layouts/_markup/render-image.html` | Serves those images from the site's own origin. An image anywhere else in the repository is a build warning: GitHub's raw host is not served to visitors while the repository is private, and the content security policy does not allow it |

### One-time setup in the Cloudflare dashboard

The website (`www/`, kudurru.io) is a separate Workers Builds project; this is
the second one. `kudurru.io` must be a zone on the same Cloudflare account.

1. **Connect the repository.** Workers & Pages → Create → Import a repository
   → `robertherbaugh/kudurru`, with these build settings:

   | Setting | Value |
   |---|---|
   | Project name | `kudurru-docs` (must match `name` in `wrangler.jsonc`) |
   | Production branch | `release`, so the site always shows the released version |
   | Root directory | `site` |
   | Build command | `make -C .. docs-site HUGO_FLAGS="--minify --gc --cleanDestinationDir --panicOnWarning --baseURL https://docs.kudurru.io/"` |
   | Deploy command | `bunx wrangler@4.149.0 deploy` |
   | Non-production branch deploy command | `bunx wrangler@4.149.0 versions upload` (a preview URL per branch and pull request) |
   | Build watch paths, include | `site/*`, `docs/*`, `api/openapi.json`, `internal/api/scalar/*`, `CHANGELOG.md`, `SECURITY.md`, `CONTRIBUTING.md` |
   | Build variables | `HUGO_PARAMS_REPOREF=release`, `BUN_VERSION=1.4.3`, `SKIP_DEPENDENCY_INSTALL=1` |

   The build reads `docs/`, `api/` and the root markdown files outside the
   root directory; Workers Builds clones the whole repository. `make` downloads
   the pinned Hugo release into `bin/` and checks its SHA-256 (it needs `curl`,
   `tar`, `gunzip` and `git`, all in the build image). `HUGO_PARAMS_REPOREF`
   is the ref that the site's links to repository files point at.

2. **Domain.** The first deploy attaches `docs.kudurru.io` as the Worker's
   custom domain (`routes` in `wrangler.jsonc`) and creates its DNS record and
   certificate. Remove any earlier `CNAME docs` record that points at GitHub
   Pages first, and leave GitHub Pages turned off for this repository. The
   website's Bulk Redirect list (`www/README.md`) does not cover this name.

3. **Check it.** Open https://docs.kudurru.io/guide/user-guide/: the
   screenshots load. Open `/reference/api/explorer/`: the API reference
   renders. Open a path that does not exist: the 404 page answers with status 404.

Until the repository is public, a link from the site to a repository file
(the "Source" line of a page, and links to files that are not pages) opens a
GitHub page that needs a sign-in. The site itself, including every image,
does not depend on the repository.

The Hugo build writes to `site/public/`, which is not committed.
