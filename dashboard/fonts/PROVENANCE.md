# Dashboard font provenance

The Kudurru theme self-hosts unmodified Latin WOFF2 subsets from these exact
Fontsource packages. `dashboard/bun.lock` pins archive integrity; no font
service is contacted at runtime. Other scripts use the existing system
fallbacks. Both normal and italic variable faces include weights 400, 500,
600 and 700 used by Astryx (Inter range 100–900; JetBrains Mono 100–800).
`font-display: swap` keeps fallback text visible while loading or offline.

| Font | Package | Upstream | License |
|---|---|---|---|
| Inter | `@fontsource-variable/inter@5.3.0` (Google Fonts v20) | https://github.com/rsms/inter | SIL OFL 1.1; Copyright 2016 The Inter Project Authors |
| JetBrains Mono | `@fontsource-variable/jetbrains-mono@5.3.0` (Google Fonts v24) | https://github.com/JetBrains/JetBrainsMono | SIL OFL 1.1; Copyright 2020 The JetBrains Mono Project Authors |

Fontsource distributes the files at https://github.com/fontsource/font-files
under `fonts/variable/inter` and `fonts/variable/jetbrains-mono`.
The package LICENSE files are included verbatim in the generated, embedded
`third-party-licenses.txt` by `scripts/licenses.ts`. The font licenses do not
change the license of the dashboard or its branding.

`src/styles.css` declares only these four files. Vite gives them hashed,
relative asset URLs, so the Go dashboard handler serves them under its
configured path prefix and unchanged `font-src 'self'` policy. The initial
bundle budget includes all four files, including italic faces whether or
not the first page uses them. No local OS font is substituted in the font
source declaration; the fallback stack is a separate theme setting.

SHA-256 of the package files (before Vite's filename hashing):

| File | SHA-256 |
|---|---|
| `inter-latin-wght-normal.woff2` | `3100e775e8616cd2611beecfa23a4263d7037586789b43f035236a2e6fbd4c62` |
| `inter-latin-wght-italic.woff2` | `7291b5970da2237441273c03b424a504b70b18f09791473fab99687dcc314720` |
| `jetbrains-mono-latin-wght-normal.woff2` | `18be452724bfdc236c074ca94a249a7f41a86752c7d04ab258ce9ed5651f6a7e` |
| `jetbrains-mono-latin-wght-italic.woff2` | `a8afa085e9ca5e53434e2ee918ba6b65c7dd4dda56509976b36591478c99d62e` |
