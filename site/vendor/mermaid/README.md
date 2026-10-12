# Mermaid browser bundle (MIT)

`mermaid.min.js.gz` is `dist/mermaid.min.js` of the npm package `mermaid`
**11.17.2**, gzipped with `gzip -9 -n`. `make docs-assets` decompresses it
into `site/.generated/mermaid/`, and the docs site serves it, fingerprinted,
only on pages that contain a Mermaid diagram (the architecture tour,
`docs/architecture/`). The site loads nothing from a CDN, and the content
security policy (`site/static/_headers`) allows scripts from its own origin
only.

- Licence: MIT, `LICENSE` in this directory (Copyright (c) 2014 - 2022 Knut
  Sveidqvist), from the package; listed in the repository NOTICE. The bundle
  also contains its permissively licensed dependencies (d3 and d3-sankey,
  dagre-d3-es, cytoscape, khroma, stylis, lodash-es, dayjs, marked, katex,
  roughjs, DOMPurify under Apache-2.0, and others); the bundle's own licence
  comments are kept.
- Why 11.x and not 12.x: mermaid 12's `mermaid.min.js` compiles in the ELK
  layout engine (elkjs, EPL-2.0); the 11.x bundle does not. The diagrams use
  the default layout and need nothing from 12.x.
- Source: https://registry.npmjs.org/mermaid/-/mermaid-11.17.2.tgz
  (npm integrity `sha512-V6K3C8EBdEsPFZXSKMJe6ppQOENxuHARr9GvHX4hh47lAbhMRD9qf4oEK7LoaRQxULMa80/qt5gHO73aCleBBg==`,
  sha256 `6ad2f42c3fc26bbf9e45cbb6d11898972573ea52b33a5f4ff51952899f950ffd`).
- `mermaid.min.js` sha256 `581ed7d74bd9048d0e3a91363927d72ef22942d7722546b27f7cc29e35390eb8`.

To update: download the tarball of the new version, check that its
`dist/mermaid.min.js` contains no ELK (`grep -c org.eclipse.elk` is 0) and
no other non-permissive code, replace `mermaid.min.js.gz` and `LICENSE`, and
change the version and hashes here and in the NOTICE.
