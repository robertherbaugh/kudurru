// Bundle budget (`bun run build` runs it after vite build): the initial
// load is index.html plus every script, module preload and stylesheet it
// references, including their CSS assets. Reports raw and gzip sizes of the initial load and of the
// whole bundle, and fails when the initial gzip size exceeds the budget.
//
//   KUDURRU_WEB_BUDGET_KB=600 bun scripts/budget.ts [dist]
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { gzipSync } from 'node:zlib';

const dist = process.argv[2] ?? 'dist';
const budgetKB = Number(process.env.KUDURRU_WEB_BUDGET_KB ?? '600');

function gz(buf: Buffer): number {
  return gzipSync(buf, { level: 9 }).length;
}

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

const html = readFileSync(join(dist, 'index.html'), 'utf8');
const refs = new Set<string>();
for (const m of html.matchAll(/(?:src|href)="\.?\/?(assets\/[^"]+)"/g)) {
  if (m[1]) refs.add(m[1]);
}
if (refs.size === 0) {
  console.error(`budget: ${dist}/index.html references no assets (placeholder page?)`);
  process.exit(1);
}

// Fonts are fetched through CSS, not directly by index.html. Include every
// referenced asset conservatively (even an italic face not used on this page)
// so self-hosting cannot silently bypass the initial-load budget.
for (const ref of refs) {
  if (!ref.endsWith('.css')) continue;
  const css = readFileSync(join(dist, ref), 'utf8');
  for (const match of css.matchAll(/url\(\s*['"]?([^)'"\s]+)['"]?\s*\)/g)) {
    const url = match[1]!;
    if (/^(?:data:|https?:|#)/i.test(url)) continue;
    refs.add(join(dirname(ref), url));
  }
}

const kb = (n: number) => `${(n / 1024).toFixed(1)} KiB`;
let initialRaw = Buffer.byteLength(html);
let initialGz = gz(Buffer.from(html));
const rows: [string, number, number][] = [];
for (const r of refs) {
  const buf = readFileSync(join(dist, r));
  // WOFF2 is already compressed and the Go handler serves its original bytes.
  const g = r.endsWith('.woff2') ? buf.length : gz(buf);
  initialRaw += buf.length;
  initialGz += g;
  rows.push([r, buf.length, g]);
}

let allRaw = 0;
let allGz = 0;
for (const f of walk(dist)) {
  const buf = readFileSync(f);
  allRaw += buf.length;
  allGz += gz(buf);
}

console.log('initial load (index.html + referenced assets and CSS dependencies; fonts served raw):');
for (const [name, raw, g] of rows.sort((a, b) => b[2] - a[2])) {
  console.log(`  ${name.padEnd(48)} ${kb(raw).padStart(11)} raw ${kb(g).padStart(11)} transferred`);
}
console.log(`  ${'total'.padEnd(48)} ${kb(initialRaw).padStart(11)} raw ${kb(initialGz).padStart(11)} transferred (budget ${budgetKB} KiB)`);
console.log(`whole bundle (${relative(process.cwd(), dist)}): ${kb(allRaw)} raw, ${kb(allGz)} gzip`);

if (initialGz > budgetKB * 1024) {
  console.error(`budget: the initial load is ${kb(initialGz)} transferred, over the ${budgetKB} KiB budget`);
  process.exit(1);
}
