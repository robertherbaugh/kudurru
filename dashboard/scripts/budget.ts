// Bundle budget (`bun run build` runs it after vite build): the initial
// load is index.html plus every script, module preload and stylesheet it
// references. Reports raw and gzip sizes of the initial load and of the
// whole bundle, and fails when the initial gzip size exceeds the budget.
//
//   KUDURRU_WEB_BUDGET_KB=600 bun scripts/budget.ts [dist]
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative } from 'node:path';
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

const kb = (n: number) => `${(n / 1024).toFixed(1)} KiB`;
let initialRaw = Buffer.byteLength(html);
let initialGz = gz(Buffer.from(html));
const rows: [string, number, number][] = [];
for (const r of refs) {
  const buf = readFileSync(join(dist, r));
  const g = gz(buf);
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

console.log('initial load (index.html + referenced assets):');
for (const [name, raw, g] of rows.sort((a, b) => b[2] - a[2])) {
  console.log(`  ${name.padEnd(48)} ${kb(raw).padStart(11)} raw ${kb(g).padStart(11)} gzip`);
}
console.log(`  ${'total'.padEnd(48)} ${kb(initialRaw).padStart(11)} raw ${kb(initialGz).padStart(11)} gzip (budget ${budgetKB} KiB)`);
console.log(`whole bundle (${relative(process.cwd(), dist)}): ${kb(allRaw)} raw, ${kb(allGz)} gzip`);

if (initialGz > budgetKB * 1024) {
  console.error(`budget: the initial load is ${kb(initialGz)} gzip, over the ${budgetKB} KiB budget`);
  process.exit(1);
}
