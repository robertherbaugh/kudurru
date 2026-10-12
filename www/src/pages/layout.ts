import { html, type Html } from "../html.ts";
import { DESCRIPTION, DOCS_URL, FOOTER, NAV, REPO_URL, SITE_URL, TAGLINE } from "../content.ts";

// The hashed files of one build, as URL paths.
export interface Assets {
  css: string;
  js: string;
  preload: string[];
  file: (name: string) => string;
}

export interface Page {
  path: string;
  title: string;
  description?: string;
  body: Html;
  assets: Assets;
  // The 404 page is not indexed and has no canonical URL.
  index?: boolean;
}

export const icon = {
  arrow: html`<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M3 8h9M8.5 4.5 12 8l-3.5 3.5" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="square"/></svg>`,
  out: html`<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M5 11 11 5M6 4.5h5.5V10" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="square"/></svg>`,
  github: html`<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8Z"/></svg>`,
  copy: html`<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M5.5 5.5h7v7h-7zM3.5 10.5v-7h7" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`,
  menu: html`<svg class="i" viewBox="0 0 16 16" aria-hidden="true"><path d="M2 5h12M2 11h12" fill="none" stroke="currentColor" stroke-width="1.5"/></svg>`,
};

export function external(href: string, label: Html | string, cls = ""): Html {
  return cls
    ? html`<a class="${cls}" href="${href}" rel="noopener">${label}</a>`
    : html`<a href="${href}" rel="noopener">${label}</a>`;
}

function header(assets: Assets, home: boolean): Html {
  const prefix = home ? "" : "/";
  return html`<header class="top">
  <div class="wrap top-row">
    <a class="brand" href="/" aria-label="kudurru home">
      <img src="${assets.file("kudurru-logo-mixed-paper.png")}" width="192" height="64" alt="kudurru">
    </a>
    <button class="menu-toggle" type="button" aria-expanded="false" aria-controls="site-nav">${icon.menu}<span>Menu</span></button>
    <nav id="site-nav" class="nav" aria-label="Primary">
      <ul class="nav-anchors">
        ${NAV.map((l) => html`<li><a href="${prefix}${l.href}">${l.label}</a></li>`)}
      </ul>
      <ul class="nav-out">
        <li>${external(DOCS_URL, html`Docs ${icon.out}`)}</li>
        <li>${external(REPO_URL, html`${icon.github}<span>GitHub</span>`, "gh")}</li>
      </ul>
    </nav>
  </div>
</header>`;
}

function footer(assets: Assets, version: string): Html {
  return html`<footer class="foot">
  <div class="wrap foot-grid">
    <div class="foot-brand">
      <img src="${assets.file("kudurru-logo-mixed-paper-tagline.png")}" width="360" height="150" alt="kudurru, ${TAGLINE}" loading="lazy">
      <p class="mono dim">koo-DOO-roo · v${version}</p>
    </div>
    ${FOOTER.map(
      (col) => html`<div class="foot-col">
      <p class="label">${col.title}</p>
      <ul>${col.links.map((l) => html`<li>${external(l.href, l.label)}</li>`)}</ul>
    </div>`,
    )}
  </div>
  <div class="wrap foot-legal">
    <p>kudurru is source-available under the PolyForm Noncommercial License 1.0.0: personal use is free, businesses need an agreement. dnsdist and keepalived are GPL-2.0 and ship only as separate, unmodified programs in their own images.</p>
    <p class="mono"><a href="/">kudurru.io</a> · ${external(DOCS_URL, "docs.kudurru.io")} · <a href="/licenses/">font licenses</a></p>
  </div>
</footer>`;
}

export function layout(page: Page, version: string): string {
  const { assets } = page;
  const description = page.description ?? DESCRIPTION;
  const url = SITE_URL + page.path;
  const index = page.index !== false;
  const doc = html`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${page.title}</title>
<meta name="description" content="${description}">
${index ? html`<link rel="canonical" href="${url}">` : html`<meta name="robots" content="noindex">`}
<meta name="theme-color" content="#182522">
<meta name="color-scheme" content="dark">
<meta property="og:type" content="website">
<meta property="og:site_name" content="kudurru">
<meta property="og:title" content="${page.title}">
<meta property="og:description" content="${description}">
<meta property="og:url" content="${url}">
<meta property="og:image" content="${SITE_URL}/brand/kudurru-social.png">
<meta property="og:image:width" content="1200">
<meta property="og:image:height" content="630">
<meta property="og:image:alt" content="kudurru, Kubernetes-native DNS">
<meta name="twitter:card" content="summary_large_image">
<link rel="icon" href="/favicon.ico" sizes="16x16 32x32 48x48">
<link rel="icon" type="image/png" sizes="192x192" href="${assets.file("kudurru-icon-192.png")}">
<link rel="apple-touch-icon" href="${assets.file("kudurru-icon-192.png")}">
<link rel="manifest" href="/site.webmanifest">
${assets.preload.map((href) => html`<link rel="preload" href="${href}" as="font" type="font/woff2" crossorigin>`)}
<link rel="stylesheet" href="${assets.css}">
<script type="module" src="${assets.js}"></script>
</head>
<body>
<a class="skip" href="#main">Skip to content</a>
<div class="rails" aria-hidden="true"></div>
${header(assets, page.path === "/")}
<main id="main">
${page.body}
</main>
${footer(assets, version)}
</body>
</html>
`;
  return doc.value;
}
