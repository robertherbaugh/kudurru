// Builds the site into a temporary directory and checks the pages: no
// inline executable script or style (JSON-LD is authorized by CSP hashes), every local link
// and asset exists, every fragment link has a target, images have alt text
// and dimensions, and external links go where they should.
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { build } from "../build.ts";
import { DEFAULT_LICENSE_API, DOCS_URL, MIGRATION_COMPARISON, REPO_URL, SUPPORT_STARTING_ANNUAL_USD } from "../src/content.ts";
import { usd } from "../src/pricing.ts";
import { headersFile, rulesFor } from "../src/headers.ts";

let out: string;
const pages: Record<string, string> = {};
const LANDING_PAGES = ["kubernetes-dns/index.html", "network-ad-blocking/index.html", "pi-hole-alternative/index.html"];
const PAGE_PATHS = ["index.html", "404.html", "move-over/index.html", "enterprise/index.html", ...LANDING_PAGES, "license/index.html", "license/text/index.html", "licenses/index.html"];

beforeAll(async () => {
  out = await mkdtemp(join(tmpdir(), "www-build-"));
  await build({ outDir: out, version: "9.8.7" });
  for (const name of PAGE_PATHS) {
    pages[name] = await readFile(join(out, name), "utf8");
  }
});

afterAll(() => rm(out, { recursive: true, force: true }));

const attrs = (page: string, attr: string) => [...page.matchAll(new RegExp(`\\s${attr}="([^"]*)"`, "g"))].map((m) => m[1]!);
const jsonLd = (page: string) => [...page.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)].map((m) => m[1]!);
const scriptHash = (text: string) => "sha256-" + createHash("sha256").update(text).digest("base64");

describe.each(PAGE_PATHS)("%s", (name) => {
  test("only JSON-LD data blocks may be inline; no styles or event handlers", () => {
    const page = pages[name]!;
    for (const tag of page.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)) {
      if (tag[0].startsWith('<script type="application/ld+json">')) {
        expect(JSON.parse(tag[1]!)["@context"]).toBe("https://schema.org");
        continue;
      }
      expect(tag[0]).toContain(' src="');
      expect(tag[1]!.trim()).toBe("");
    }
    expect(page).not.toMatch(/<style\b/);
    expect(page).not.toMatch(/\sstyle="/);
    expect(page).not.toMatch(/\son[a-z]+="/);
    expect(page).not.toContain("javascript:");
  });

  test("local links and assets exist", async () => {
    const page = pages[name]!;
    const refs = [...attrs(page, "href"), ...attrs(page, "src")].filter((r) => r.startsWith("/"));
    expect(refs.length).toBeGreaterThan(3);
    for (const ref of refs) {
      const path = ref.split("#")[0]!.split("?")[0]!;
      const file = path.endsWith("/") ? path + "index.html" : path;
      expect((await stat(join(out, file))).isFile()).toBe(true);
    }
  });

  test("fragment links have targets on their page or the home page", () => {
    const home = pages["index.html"]!;
    for (const ref of attrs(pages[name]!, "href").filter((r) => r.includes("#"))) {
      const [path, frag] = ref.split("#");
      if (path !== "" && path !== "/") continue;
      const ids = new Set(attrs(path === "" ? pages[name]! : home, "id"));
      expect(ids.has(frag!)).toBe(true);
    }
  });

  test("ids are unique and ARIA references resolve", () => {
    const page = pages[name]!;
    const ids = attrs(page, "id");
    expect(new Set(ids).size).toBe(ids.length);
    for (const attr of ["aria-controls", "aria-labelledby"]) {
      for (const ref of attrs(page, attr)) expect(ids).toContain(ref);
    }
  });

  test("images have alt text and dimensions", () => {
    for (const img of pages[name]!.matchAll(/<img\b[^>]*>/g)) {
      expect(img[0]).toMatch(/\salt="[^"]*"/);
      expect(img[0]).toMatch(/\swidth="\d+"/);
      expect(img[0]).toMatch(/\sheight="\d+"/);
    }
  });

  test("external links are https and open safely", () => {
    for (const a of pages[name]!.matchAll(/<a\b[^>]*href="(https?:[^"]*)"[^>]*>/g)) {
      expect(a[1]!.startsWith("https://")).toBe(true);
      expect(a[0]).toContain('rel="noopener"');
    }
  });
});

test("the home page links to the docs and the repository and carries the version", () => {
  const home = pages["index.html"]!;
  expect(home).toContain(`href="${DOCS_URL}"`);
  expect(home).toContain(`href="${REPO_URL}"`);
  expect(home).toContain("v9.8.7");
  expect(home).toContain("ghcr.io/robertherbaugh/kudurru:9.8.7");
  expect(home).toContain('<link rel="canonical" href="https://kudurru.io/">');
});

test("the 404 page is not indexed", () => {
  expect(pages["404.html"]).toContain('<meta name="robots" content="noindex">');
  expect(pages["404.html"]).not.toContain('rel="canonical"');
});

test("the build writes _headers for Cloudflare", async () => {
  const hashes = [...new Set(Object.values(pages).flatMap(jsonLd).map(scriptHash))];
  const headers = await readFile(join(out, "_headers"), "utf8");
  expect(headers).toBe(headersFile(rulesFor(DEFAULT_LICENSE_API, hashes)));
  expect(headers).not.toContain("unsafe-inline");
  for (const line of headers.split("\n")) expect(line.length).toBeLessThan(2000);
});

describe("search discovery", () => {
  const indexed = PAGE_PATHS.filter((name) => name !== "404.html");

  test("every indexable page has a unique title, description and matching canonical and social URLs", () => {
    const titles: string[] = [];
    const descriptions: string[] = [];
    for (const name of indexed) {
      const page = pages[name]!;
      const url = "https://kudurru.io/" + name.replace(/index\.html$/, "");
      const title = /<title>([^<]+)<\/title>/.exec(page)?.[1];
      const description = /<meta name="description" content="([^"]+)">/.exec(page)?.[1];
      expect(title).toBeTruthy();
      expect(description).toBeTruthy();
      expect(page.match(/<title>/g)?.length).toBe(1);
      expect(page.match(/<h1\b/g)?.length).toBe(1);
      expect(page).toContain(`<link rel="canonical" href="${url}">`);
      expect(page).toContain(`<meta property="og:url" content="${url}">`);
      expect(page).toContain(`<meta property="og:title" content="${title}">`);
      expect(page).toContain(`<meta name="twitter:title" content="${title}">`);
      expect(page).toContain(`<meta name="twitter:description" content="${description}">`);
      expect(page).toContain('<meta name="twitter:image" content="https://kudurru.io/brand/kudurru-social.png">');
      expect(page).toContain("max-image-preview:large");
      titles.push(title!);
      descriptions.push(description!);
    }
    expect(new Set(titles).size).toBe(indexed.length);
    expect(new Set(descriptions).size).toBe(indexed.length);
  });

  test("the sitemap contains exactly the indexable canonical pages, and robots advertises it", async () => {
    const sitemap = await readFile(join(out, "sitemap.xml"), "utf8");
    const locations = [...sitemap.matchAll(/<loc>([^<]+)<\/loc>/g)].map((match) => match[1]!);
    expect(locations.sort()).toEqual(indexed.map((name) => "https://kudurru.io/" + name.replace(/index\.html$/, "")).sort());
    expect(await readFile(join(out, "robots.txt"), "utf8")).toBe("User-agent: *\nAllow: /\n\nSitemap: https://kudurru.io/sitemap.xml\n");
  });

  test("JSON-LD describes the canonical site and software without invented prices or ratings", () => {
    for (const name of indexed) {
      const blocks = jsonLd(pages[name]!);
      expect(blocks.length).toBe(1);
      const graph = JSON.parse(blocks[0]!)["@graph"] as {
        "@type": string;
        url?: string;
        softwareVersion?: string;
        license?: string;
        itemListElement?: { position: number; item: string }[];
      }[];
      const url = "https://kudurru.io/" + name.replace(/index\.html$/, "");
      expect(graph.find((item) => item["@type"] === "WebPage")?.url).toBe(url);
      expect(graph.find((item) => item["@type"] === "WebSite")?.url).toBe("https://kudurru.io/");
      expect(blocks[0]).not.toMatch(/"(?:offers|aggregateRating|review)"/);
      if (name !== "index.html") {
        const crumbs = graph.find((item) => item["@type"] === "BreadcrumbList")?.itemListElement ?? [];
        expect(crumbs[0]?.item).toBe("https://kudurru.io/");
        expect(crumbs.at(-1)?.item).toBe(url);
        expect(crumbs.map((item) => item.position)).toEqual(crumbs.map((_, i) => i + 1));
      } else {
        const software = graph.find((item) => item["@type"] === "SoftwareApplication");
        expect(software?.softwareVersion).toBe("9.8.7");
        expect(software?.license).toBe("https://kudurru.io/license/text/");
      }
    }
    expect(jsonLd(pages["404.html"]!)).toEqual([]);
  });

  test.each(LANDING_PAGES)("%s structured breadcrumb matches the visible trail", (name) => {
    const page = pages[name]!;
    const visibleLabel = /aria-current="page">([^<]+)<\/span>/.exec(page)?.[1];
    expect(visibleLabel).toBeTruthy();
    const graph = JSON.parse(jsonLd(page)[0]!)["@graph"] as {
      "@type": string;
      itemListElement?: { name: string }[];
    }[];
    const trail = graph.find((item) => item["@type"] === "BreadcrumbList")?.itemListElement;
    expect(trail?.at(-1)?.name).toBe(visibleLabel);
  });

  test.each(LANDING_PAGES)("%s is discoverable and sends readers to maintained guides", async (name) => {
    const path = "/" + name.replace(/index\.html$/, "");
    expect(attrs(pages["index.html"]!, "href")).toContain(path);
    for (const page of Object.values(pages)) {
      const footer = /<footer\b[^>]*>([\s\S]*?)<\/footer>/.exec(page)?.[1] ?? "";
      expect(attrs(footer, "href")).toContain(path);
    }
    const main = /<main\b[^>]*>([\s\S]*?)<\/main>/.exec(pages[name]!)![1]!;
    const docsLinks = new Set(attrs(main, "href").filter((href) => href.startsWith(DOCS_URL + "/")));
    expect(docsLinks.size).toBeGreaterThan(2);
    for (const href of docsLinks) {
      const path = new URL(href).pathname.replace(/\/$/, "");
      const source = path.startsWith("/start/") || path.startsWith("/migrate/") || path.startsWith("/reference/crds/")
        ? `site/content${path}.md`
        : `docs${path}.md`;
      // Section roots such as /reference/crds/ use Hugo's _index.md.
      const sourcePath = path === "/reference/crds" ? "site/content/reference/crds/_index.md" : source;
      expect((await stat(join(import.meta.dir, "../..", sourcePath))).isFile()).toBe(true);
    }
  });

  test("preview builds opt out of indexing in both HTML and response headers", async () => {
    const dir = await mkdtemp(join(tmpdir(), "www-preview-"));
    try {
      await build({ outDir: dir, version: "9.8.7", noIndex: true });
      for (const name of PAGE_PATHS) {
        const page = await readFile(join(dir, name), "utf8");
        expect(page).toContain('<meta name="robots" content="noindex">');
        expect(page).not.toContain('rel="canonical"');
        expect(jsonLd(page)).toEqual([]);
      }
      expect(await readFile(join(dir, "_headers"), "utf8")).toContain("  X-Robots-Tag: noindex\n");
      expect(await readFile(join(dir, "sitemap.xml"), "utf8")).not.toContain("<loc>");
      expect(await readFile(join(dir, "robots.txt"), "utf8")).toBe("User-agent: *\nAllow: /\n");
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

test("fonts are separate hashed files, not data URLs", async () => {
  const css = attrs(pages["index.html"]!, "href").find((h) => h.endsWith(".css"))!;
  const text = await readFile(join(out, css), "utf8");
  expect(text).not.toContain("data:font");
  const fonts = [...text.matchAll(/url\((\/assets\/[^)]+\.woff2)\)/g)].map((m) => m[1]!);
  expect(fonts.length).toBe(3);
  for (const f of fonts) expect((await stat(join(out, f))).size).toBeGreaterThan(10_000);
});

describe("the move-over page", () => {
  test("is linked from every page's nav and footer and listed in the sitemap", async () => {
    for (const name of PAGE_PATHS) {
      const page = pages[name]!;
      const nav = /<nav id="site-nav"[^>]*>([\s\S]*?)<\/nav>/.exec(page)?.[1] ?? "";
      const footer = /<footer\b[^>]*>([\s\S]*?)<\/footer>/.exec(page)?.[1] ?? "";
      expect(attrs(nav, "href")).toContain("/move-over/");
      expect(attrs(footer, "href")).toContain("/move-over/");
    }
    expect(await readFile(join(out, "sitemap.xml"), "utf8")).toContain("<loc>https://kudurru.io/move-over/</loc>");
  });

  test("renders all five comparison columns and every feature row", () => {
    const page = pages["move-over/index.html"]!;
    const headings = [...page.matchAll(/<th scope="col">([^<]*)<\/th>/g)].map((match) => match[1]!);
    expect(headings).toEqual(["Feature", "Pi-hole", "AdGuard Home", "Technitium", "kudurru v0.1.0"]);
    const body = /<tbody>([\s\S]*?)<\/tbody>/.exec(page)?.[1] ?? "";
    const rows = [...body.matchAll(/<tr>([\s\S]*?)<\/tr>/g)];
    expect(rows).toHaveLength(MIGRATION_COMPARISON.length);
    for (const row of rows) {
      expect(row[1]!.match(/<th scope="row">/g)).toHaveLength(1);
      expect(row[1]!.match(/<td>/g)).toHaveLength(4);
    }
  });

  test("offers the comparison and all three migration guides in the home-page section", () => {
    const section = /<section\b[^>]*aria-labelledby="home-migrate-title"[^>]*>([\s\S]*?)<\/section>/.exec(pages["index.html"]!)?.[1] ?? "";
    const links = attrs(section, "href");
    expect(links).toContain("/move-over/");
    for (const slug of ["pihole", "adguard-home", "technitium"]) {
      expect(links).toContain(`${DOCS_URL}/migrate/${slug}/`);
    }
  });
});

describe("the enterprise page", () => {
  test("is linked from the nav and footer of every page and listed in the sitemap", async () => {
    for (const name of PAGE_PATHS) {
      const page = pages[name]!;
      const nav = /<nav id="site-nav"[^>]*>([\s\S]*?)<\/nav>/.exec(page)?.[1] ?? "";
      const footer = /<footer\b[^>]*>([\s\S]*?)<\/footer>/.exec(page)?.[1] ?? "";
      expect(attrs(nav, "href")).toContain("/enterprise/");
      expect(attrs(footer, "href")).toContain("/enterprise/");
    }
    expect(await readFile(join(out, "sitemap.xml"), "utf8")).toContain("<loc>https://kudurru.io/enterprise/</loc>");
  });

  test("publishes both annual editions, a static calculator example and volume rules", () => {
    const page = pages["enterprise/index.html"]!;
    expect(page).toContain("$99<span>/year platform fee</span>");
    expect(page).toContain("+ $99 / node / year");
    expect(page).toContain("$499<span>/year platform fee</span>");
    expect(page).toContain("+ $299 / node / year");
    expect(page).toContain("billed annually in USD");
    expect(page).toContain('data-pricing-result>$1,994</p>');
    expect(page).toContain("$166.17/month, billed annually");
    expect(page).toContain("$2,326.50");
    expect(page).toContain("$7,226.50");
    expect(page).toContain("quoted individually");
    expect(page).toContain("Community / Homelab");
    expect(page).toContain("government production use");
    expect(page).toContain("DNS keeps answering.");
    expect(page).not.toMatch(/TBD|Proposed pricing|placeholders/);
  });

  test("offers an email contact for discussing an enterprise deployment", () => {
    expect(attrs(pages["enterprise/index.html"]!, "href")).toContain("mailto:hello@kudurru.io?subject=kudurru%20Enterprise");
  });

  test("keeps the public support starting rate aligned with the enterprise docs", async () => {
    const rate = `${usd(SUPPORT_STARTING_ANNUAL_USD)}/year`;
    expect(pages["enterprise/index.html"]).toContain(`assistance starts at ${rate}`);
    const docs = await readFile(join(import.meta.dir, "../../docs/enterprise.md"), "utf8");
    expect(docs).toContain(`assistance starts at **${rate}**`);
  });

  test("its documentation links point to existing repository sources", async () => {
    const main = /<main\b[^>]*>([\s\S]*?)<\/main>/.exec(pages["enterprise/index.html"]!)?.[1] ?? "";
    const links = new Set(attrs(main, "href").filter((href) => href.startsWith(`${DOCS_URL}/`)));
    expect(links.size).toBeGreaterThan(0);
    // Hugo mounts this agreement from the repository root (site/hugo.yaml).
    const mountedSources: Record<string, string> = {
      "/project/commercial-license/": "COMMERCIAL_LICENSE_AGREEMENT.md",
    };
    for (const href of links) {
      const path = new URL(href).pathname;
      const source = mountedSources[path] ?? `docs${path.replace(/\/$/, "")}.md`;
      expect((await stat(join(import.meta.dir, "../..", source))).isFile()).toBe(true);
    }
  });
});

describe("the license page", () => {
  test("states the terms, the free limits and where to write", () => {
    const page = pages["license/index.html"]!;
    expect(page).toContain("Kudurru Community License");
    expect(page).toContain("DNS never depends on a key");
    expect(page).toContain("mailto:hello@kudurru.io?subject=Free%20personal%20key");
    expect(page).toContain("mailto:hello@kudurru.io?subject=kudurru%20commercial%20license");
    expect(page).not.toContain("Apache-2.0");
    // One row per row of the table in content.ts, each with its three cells.
    expect(page.match(/<tr><th scope="row">/g)?.length).toBe(11);
  });

  test("without a key a node has one policy, the default one", () => {
    const page = pages["license/index.html"]!;
    expect(page).toContain("One node, the default policy,");
    expect(page).toContain('<tr><th scope="row">Policies</th><td>1, the default policy</td><td>Unlimited</td><td>Unlimited</td></tr>');
    expect(page).not.toContain("two policies");
    expect(pages["index.html"]).toContain("a policy beyond the default needs a license key");
  });

  test("below the intro it spans the full width, like its table: only the form's box is narrower", () => {
    const page = pages["license/index.html"]!;
    expect(page).toMatch(/<section class="sec" aria-labelledby="lic-title">\s*<div class="wrap narrow">/);
    expect(page.match(/class="wrap narrow"/g)?.length).toBe(1);
    expect(page).toMatch(/<section class="sec lic-sec" aria-labelledby="lic-get-title">\s*<div class="wrap">\s*<form class="licform lic-row"/);
    for (const id of ["lic-req-title", "lic-get-title", "lic-read-title"]) expect(page).toContain(`<h2 id="${id}">`);
    // Using a key on a node is the docs' job, not this page's.
    expect(page).not.toContain("lic-use-title");
    expect(page).toContain('<div class="licform-box">');
  });

  test("is linked from the footer of every page and listed in the sitemap", async () => {
    for (const name of Object.keys(pages)) expect(pages[name]).toContain('href="/license/"');
    expect(await readFile(join(out, "sitemap.xml"), "utf8")).toContain("https://kudurru.io/license/");
  });
});

describe("the key request form", () => {
  test("is on the license page only, posts to the license server's public endpoint, and is hidden without script", () => {
    const page = pages["license/index.html"]!;
    expect(page).toContain("data-license-form");
    expect(page).toContain('data-endpoint="https://licenses.kudurru.io/api/v1/public/key-requests"');
    expect(page).not.toMatch(/<form[^>]*\saction=/); // the CSP has form-action 'none'; submission is script-driven
    expect(pages["index.html"]).not.toContain("data-license-form");
  });

  test("has the fields the personal and the business request need", () => {
    const page = pages["license/index.html"]!;
    for (const name of ["kind", "name", "email", "personal-use", "noncommercial", "company", "business-nodes", "business-use", "website"]) {
      expect(page).toContain(`name="${name}"`);
    }
    // The server's limits are on the inputs too.
    expect(page).toMatch(/name="personal-use"[^>]*maxlength="1000"/);
    expect(page).toMatch(/name="name"[^>]*maxlength="200"/);
    // The honeypot is out of the accessibility tree and the tab order.
    expect(page).toMatch(/aria-hidden="true"><label>Leave this empty <input type="text" name="website" tabindex="-1"/);
  });

  test("the checkbox is the certification, with a link to the license text", () => {
    const page = pages["license/index.html"]!;
    expect(page).toContain("I agree and certify this key will only be used for eligible personal, qualifying nonprofit or community-development use in accordance with the license (");
    expect(page).toContain('<a href="/license/text/" target="_blank" rel="noopener">Kudurru Community License</a>).');
  });

  test("says that a personal key is approved before it is mailed", () => {
    expect(pages["license/index.html"]).toContain("After you confirm, we review the request and email your key once it is approved.");
  });

  test("says where a business request goes", () => {
    const page = pages["license/index.html"]!;
    expect(page).toContain("opens an email to hello@kudurru.io with the subject Business License");
  });

  test("the headers file lets only the form's origin through connect-src", async () => {
    const headers = await readFile(join(out, "_headers"), "utf8");
    expect(headers).toContain("connect-src 'self' https://licenses.kudurru.io;");
  });

  test("WWW_LICENSE_API is honored and checked", async () => {
    const dir = await mkdtemp(join(tmpdir(), "www-api-"));
    try {
      await build({ outDir: dir, version: "9.8.7", licenseApi: "https://licenses.example.org" });
      expect(await readFile(join(dir, "license/index.html"), "utf8")).toContain('data-endpoint="https://licenses.example.org/api/v1/public/key-requests"');
      expect(await readFile(join(dir, "_headers"), "utf8")).toContain("connect-src 'self' https://licenses.example.org;");
      await expect(build({ outDir: dir, version: "9.8.7", licenseApi: "not a url" })).rejects.toThrow();
    } finally {
      await rm(dir, { recursive: true, force: true });
    }
  });
});

describe("the license text", () => {
  test("is the repository's LICENSE, rendered, with working anchors", async () => {
    const page = pages["license/text/index.html"]!;
    const source = await readFile(join(import.meta.dir, "../../LICENSE"), "utf8");
    expect(page).toContain("<h1");
    expect(page).toContain("Kudurru Community License");
    for (const heading of source.matchAll(/^## (.+)$/gm)) expect(page).toContain(`>${heading[1]}</h2>`);
    const ids = new Set(attrs(page, "id"));
    for (const ref of attrs(page, "href").filter((r) => r.startsWith("/license/text/#"))) expect(ids.has(ref.split("#")[1]!)).toBe(true);
    expect(page).toContain("Required Notice: Copyright 2026 Robert Herbaugh");
  });

  test("is offered as a plain text download, byte for byte", async () => {
    const source = await readFile(join(import.meta.dir, "../../LICENSE"), "utf8");
    expect(await readFile(join(out, "license/text/Kudurru-Community-License.txt"), "utf8")).toBe(source);
    expect(pages["license/text/index.html"]).toContain('href="/license/text/Kudurru-Community-License.txt"');
  });

  test("is linked from the footer of every page, from the certification and from the license page, and listed in the sitemap", async () => {
    for (const name of Object.keys(pages)) expect(pages[name]).toContain('<a href="/license/text/">Kudurru Community License</a>');
    expect(pages["license/index.html"]).toContain("https://docs.kudurru.io/project/licensing/");
    expect(pages["license/index.html"]).toContain("https://docs.kudurru.io/project/license/");
    expect(await readFile(join(out, "sitemap.xml"), "utf8")).toContain("https://kudurru.io/license/text/");
  });
});

describe("the navigation", () => {
  test("links the license page from every page, and never builds a path with two slashes", () => {
    for (const name of Object.keys(pages)) {
      const nav = /<nav id="site-nav"[^>]*>([\s\S]*?)<\/nav>/.exec(pages[name]!)?.[1] ?? "";
      expect(nav).toContain('href="/license/">License</a>');
      expect(nav).not.toContain('href="//');
    }
    // The anchors still lead to the home page sections from the other pages.
    const nav404 = /<nav id="site-nav"[^>]*>([\s\S]*?)<\/nav>/.exec(pages["404.html"]!)?.[1] ?? "";
    expect(nav404).toContain('href="/#pipeline"');
    const navHome = /<nav id="site-nav"[^>]*>([\s\S]*?)<\/nav>/.exec(pages["index.html"]!)?.[1] ?? "";
    expect(navHome).toContain('href="#pipeline"');
  });
});
