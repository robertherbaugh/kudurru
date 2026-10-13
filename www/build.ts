// Builds the kudurru.io site into dist/ (or the directory given with
// --out): the pages rendered from src/pages, the client script and the
// stylesheet bundled by Bun with hashed names, and the brand artwork and
// dashboard screenshots copied from docs/, and the _headers file Cloudflare
// applies. `bun build.ts --watch` rebuilds on change and serves the result
// with `wrangler dev` on http://localhost:8787/, as Cloudflare will.
import { createHash } from "node:crypto";
import { mkdir, readFile, rm, writeFile, copyFile } from "node:fs/promises";
import { watch } from "node:fs";
import { basename, dirname, extname, join, resolve } from "node:path";
import { DEFAULT_LICENSE_API, LICENSE_TEXT_FILE, LICENSE_TEXT_PATH, SHOTS, SITE_URL, TAGLINE } from "./src/content.ts";
import { headersFile, rulesFor } from "./src/headers.ts";
import { layout, type Assets } from "./src/pages/layout.ts";
import { home, licenses, notFound } from "./src/pages/index.ts";
import { license, licenseText } from "./src/pages/license.ts";
import { renderMarkdown } from "./src/markdown.ts";

const ROOT = import.meta.dir;
const REPO = resolve(ROOT, "..");
const BRAND = join(REPO, "docs/brand/assets");
const SHOTS_DIR = join(REPO, "docs/guide/images");

// Brand files the pages reference by hashed name.
const BRAND_FILES = [
  "kudurru-logo-mixed-paper.png",
  "kudurru-logo-mixed-paper-tagline.png",
  "kudurru-mark-clay.png",
  "kudurru-icon-192.png",
];

const FONTS = [
  { pkg: "@fontsource-variable/inter", file: "inter-latin-wght-normal.woff2" },
  { pkg: "@fontsource-variable/inter", file: "inter-latin-ext-wght-normal.woff2" },
  { pkg: "@fontsource-variable/jetbrains-mono", file: "jetbrains-mono-latin-wght-normal.woff2" },
];

const FONT_LICENSES = [
  { name: "Inter", pkg: "@fontsource-variable/inter", file: "/licenses/inter-OFL.txt" },
  { name: "JetBrains Mono", pkg: "@fontsource-variable/jetbrains-mono", file: "/licenses/jetbrains-mono-OFL.txt" },
];

export interface BuildOptions {
  outDir: string;
  version: string;
  // The license server the "Request a key" form posts to; default
  // WWW_LICENSE_API, else DEFAULT_LICENSE_API.
  licenseApi?: string;
}

async function readVersion(): Promise<string> {
  const fromEnv = process.env.WWW_VERSION?.trim();
  if (fromEnv) return fromEnv.replace(/^v/, "");
  return (await readFile(join(REPO, "VERSION"), "utf8")).trim();
}

function hashed(name: string, bytes: Uint8Array): string {
  const h = createHash("sha256").update(bytes).digest("hex").slice(0, 10);
  const ext = extname(name);
  return `${basename(name, ext)}-${h}${ext}`;
}

async function put(path: string, data: string | Uint8Array): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  await writeFile(path, data);
}

export async function build({ outDir, version, licenseApi = process.env.WWW_LICENSE_API?.trim() || DEFAULT_LICENSE_API }: BuildOptions): Promise<void> {
  new URL(licenseApi); // fail the build on a malformed URL, not the visitor's browser
  await rm(outDir, { recursive: true, force: true });
  await mkdir(join(outDir, "assets"), { recursive: true });

  const bundle = await Bun.build({
    entrypoints: [join(ROOT, "src/client/main.ts"), join(ROOT, "src/styles/site.css")],
    outdir: join(outDir, "assets"),
    publicPath: "/assets/",
    naming: { entry: "[name]-[hash].[ext]", asset: "[name]-[hash].[ext]", chunk: "[name]-[hash].[ext]" },
    // /fonts/* are rewritten below; Bun would inline them as data URLs.
    external: ["/fonts/*"],
    target: "browser",
    minify: true,
    sourcemap: "none",
  });
  if (!bundle.success) {
    for (const log of bundle.logs) console.error(log);
    throw new Error("bundling failed");
  }
  const url = (path: string) => "/assets/" + basename(path);
  const entry = (ext: string) => {
    // Bun reports the CSS entry point as an asset; match on the extension.
    const out = bundle.outputs.find((o) => o.path.endsWith(ext));
    if (!out) throw new Error(`no ${ext} entry point in the bundle`);
    return url(out.path);
  };

  const files = new Map<string, string>();
  const copyHashed = async (dir: string, name: string) => {
    const bytes = await readFile(join(dir, name));
    const out = hashed(name, bytes);
    await writeFile(join(outDir, "assets", out), bytes);
    files.set(name, "/assets/" + out);
    return "/assets/" + out;
  };

  // Fonts: hashed copies, then the stylesheet points at them and is renamed
  // after its final content.
  let css = await readFile(join(outDir, entry(".css")), "utf8");
  await rm(join(outDir, entry(".css")));
  for (const font of FONTS) {
    const path = await copyHashed(join(ROOT, "node_modules", font.pkg, "files"), font.file);
    css = css.replaceAll(`/fonts/${font.file}`, path);
  }
  if (css.includes("/fonts/")) throw new Error("site.css names a font that build.ts does not copy");
  const cssName = hashed("site.css", new TextEncoder().encode(css));
  await writeFile(join(outDir, "assets", cssName), css);
  const preload = [files.get("inter-latin-wght-normal.woff2")!];
  for (const name of BRAND_FILES) await copyHashed(BRAND, name);
  for (const shot of SHOTS) await copyHashed(SHOTS_DIR, shot.file);

  // Stable URLs: browsers ask for /favicon.ico, and link previews and the
  // web manifest are fetched by other programs long after a deploy.
  await copyFile(join(BRAND, "favicon.ico"), join(outDir, "favicon.ico"));
  for (const name of ["kudurru-social.png", "kudurru-icon-192.png", "kudurru-icon-512.png"]) {
    await put(join(outDir, "brand", name), await readFile(join(BRAND, name)));
  }

  const assets: Assets = {
    css: "/assets/" + cssName,
    js: entry(".js"),
    preload,
    file: (name) => {
      const path = files.get(name);
      if (!path) throw new Error(`asset ${name} was not copied`);
      return path;
    },
  };

  await put(
    join(outDir, "index.html"),
    layout({ path: "/", title: `kudurru: ${TAGLINE}`, body: home(assets, version), assets }, version),
  );
  await put(
    join(outDir, "404.html"),
    layout({ path: "/404.html", title: "Not found · kudurru", body: notFound(), assets, index: false }, version),
  );
  await put(
    join(outDir, "license/index.html"),
    layout(
      {
        path: "/license/",
        title: "License and keys · kudurru",
        description: "Core software uses the Kudurru Community License. Eligible community use is free; business and government production needs an agreement. Other folders have separate licenses.",
        body: license(licenseApi),
        assets,
      },
      version,
    ),
  );
  // The license text: one source, the repository's LICENSE.
  const licenseSource = await readFile(join(REPO, "LICENSE"), "utf8");
  await put(
    join(outDir, "license/text/index.html"),
    layout(
      {
        path: LICENSE_TEXT_PATH,
        title: "Kudurru Community License · kudurru",
        description: "The license of kudurru: the Kudurru Community License, in full.",
        body: licenseText(renderMarkdown(licenseSource, LICENSE_TEXT_PATH)),
        assets,
      },
      version,
    ),
  );
  await put(join(outDir, LICENSE_TEXT_FILE), licenseSource);
  await put(
    join(outDir, "licenses/index.html"),
    layout(
      {
        path: "/licenses/",
        title: "Font licenses · kudurru",
        description: "Licenses of the fonts on kudurru.io.",
        body: licenses(FONT_LICENSES),
        assets,
      },
      version,
    ),
  );
  for (const font of FONT_LICENSES) {
    await put(join(outDir, font.file), await readFile(join(ROOT, "node_modules", font.pkg, "LICENSE")));
  }

  await put(join(outDir, "_headers"), headersFile(rulesFor(licenseApi)));
  await put(join(outDir, "robots.txt"), `User-agent: *\nAllow: /\n\nSitemap: ${SITE_URL}/sitemap.xml\n`);
  await put(
    join(outDir, "sitemap.xml"),
    `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">\n` +
      ["/", "/license/", LICENSE_TEXT_PATH, "/licenses/"].map((p) => `  <url><loc>${SITE_URL}${p}</loc></url>\n`).join("") +
      `</urlset>\n`,
  );
  await put(
    join(outDir, "site.webmanifest"),
    JSON.stringify(
      {
        name: "kudurru",
        short_name: "kudurru",
        description: "Kubernetes-native DNS",
        start_url: "/",
        display: "browser",
        background_color: "#182522",
        theme_color: "#182522",
        icons: [
          { src: "/brand/kudurru-icon-192.png", sizes: "192x192", type: "image/png" },
          { src: "/brand/kudurru-icon-512.png", sizes: "512x512", type: "image/png" },
        ],
      },
      null,
      2,
    ) + "\n",
  );
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const outIndex = args.indexOf("--out");
  const outDir = resolve(outIndex >= 0 && args[outIndex + 1] ? args[outIndex + 1]! : join(ROOT, "dist"));
  const version = await readVersion();
  const run = async () => {
    const started = performance.now();
    await build({ outDir, version });
    console.log(`built ${outDir} (v${version}) in ${Math.round(performance.now() - started)} ms`);
  };
  await run();

  if (args.includes("--watch")) {
    // wrangler dev serves dist/ with the same 404, trailing-slash and
    // _headers handling as production and picks up rebuilt files.
    const wrangler = Bun.spawn(["bunx", "wrangler", "dev"], { cwd: ROOT, stdio: ["inherit", "inherit", "inherit"] });
    let pending: ReturnType<typeof setTimeout> | undefined;
    watch(join(ROOT, "src"), { recursive: true }, () => {
      clearTimeout(pending);
      pending = setTimeout(() => run().catch((err) => console.error(err)), 120);
    });
    await wrangler.exited;
  }
}
