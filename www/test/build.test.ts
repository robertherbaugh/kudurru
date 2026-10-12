// Builds the site into a temporary directory and checks the pages: no
// inline script or style (the CSP in src/headers.ts forbids them), every local link
// and asset exists, every fragment link has a target, images have alt text
// and dimensions, and external links go where they should.
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { mkdtemp, readFile, rm, stat } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { build } from "../build.ts";
import { DOCS_URL, REPO_URL } from "../src/content.ts";
import { headersFile } from "../src/headers.ts";

let out: string;
const pages: Record<string, string> = {};

beforeAll(async () => {
  out = await mkdtemp(join(tmpdir(), "www-build-"));
  await build({ outDir: out, version: "9.8.7" });
  for (const name of ["index.html", "404.html", "licenses/index.html"]) {
    pages[name] = await readFile(join(out, name), "utf8");
  }
});

afterAll(() => rm(out, { recursive: true, force: true }));

const attrs = (page: string, attr: string) => [...page.matchAll(new RegExp(`\\s${attr}="([^"]*)"`, "g"))].map((m) => m[1]!);

describe.each(["index.html", "404.html", "licenses/index.html"])("%s", (name) => {
  test("no inline scripts, styles or event handlers", () => {
    const page = pages[name]!;
    for (const tag of page.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)) {
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

  test("fragment links have targets on the home page", () => {
    const home = pages["index.html"]!;
    const ids = new Set(attrs(home, "id"));
    for (const ref of attrs(pages[name]!, "href").filter((r) => r.includes("#"))) {
      const [path, frag] = ref.split("#");
      if (path !== "" && path !== "/") continue;
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
  expect(await readFile(join(out, "_headers"), "utf8")).toBe(headersFile());
});

test("fonts are separate hashed files, not data URLs", async () => {
  const css = attrs(pages["index.html"]!, "href").find((h) => h.endsWith(".css"))!;
  const text = await readFile(join(out, css), "utf8");
  expect(text).not.toContain("data:font");
  const fonts = [...text.matchAll(/url\((\/assets\/[^)]+\.woff2)\)/g)].map((m) => m[1]!);
  expect(fonts.length).toBe(3);
  for (const f of fonts) expect((await stat(join(out, f))).size).toBeGreaterThan(10_000);
});
