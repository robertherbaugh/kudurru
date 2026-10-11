# The kudurru docs site

A [Hugo](https://gohugo.io) site that publishes the markdown under `docs/`
and a few root files without moving or rewriting them, plus pages of its
own: the landing page, one page per role, migration guides and the
generated references.

```sh
make docs-serve        # http://localhost:1313/, rebuilds on change (PORT=... to move it)
make docs-site         # site/public, fails on any unresolved link
make docs-check-links  # the same build below /kudurru/, checked with htmltest
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

`.github/workflows/docs.yml` builds and checks the site on every pull
request. `.github/workflows/docs-pages.yml` publishes it to GitHub Pages for a
`v*` tag or when started by hand; it needs Settings → Pages → Source set to
"GitHub Actions".
