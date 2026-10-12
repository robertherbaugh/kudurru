// Pre-compresses the bundle for the Go binary (`bun run build` runs it last,
// after budget.ts has measured the raw files): every compressible file of
// at least 1 KiB under dist/ except index.html is replaced by <name>.gz
// (gzip level 9). The binary then embeds only the compressed bytes;
// web.Handler serves them with Content-Encoding: gzip and inflates a file
// once for a client that does not accept gzip. index.html stays raw: the
// handler rewrites its <base href> per path prefix. KUDURRU_WEB_BROTLI=1
// also writes <name>.br (quality 11), which the handler prefers for clients
// that accept br, at the price of embedding both forms.
//
//   bun scripts/compress.ts [dist]
import { readdirSync, readFileSync, statSync, unlinkSync, writeFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { brotliCompressSync, constants, gzipSync } from 'node:zlib';

const dist = process.argv[2] ?? 'dist';
const brotli = process.env.KUDURRU_WEB_BROTLI === '1';
// The types web.Handler compresses (handler.go compressible) and its 1 KiB
// threshold; images and fonts are compressed formats already.
const COMPRESSIBLE = /\.(js|mjs|css|html|txt|json|webmanifest|svg|wasm|map)$/;
const MIN = 1024;

function walk(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const p = join(dir, name);
    return statSync(p).isDirectory() ? walk(p) : [p];
  });
}

const kb = (n: number) => `${(n / 1024).toFixed(1)} KiB`;
let files = 0;
let raw = 0;
let gz = 0;
let br = 0;
let kept = 0;
for (const f of walk(dist)) {
  const rel = relative(dist, f);
  const buf = readFileSync(f);
  if (rel === 'index.html' || !COMPRESSIBLE.test(f) || buf.length < MIN) {
    kept += buf.length;
    continue;
  }
  const g = gzipSync(buf, { level: 9 });
  writeFileSync(`${f}.gz`, g);
  if (brotli) {
    const b = brotliCompressSync(buf, {
      params: { [constants.BROTLI_PARAM_QUALITY]: 11, [constants.BROTLI_PARAM_SIZE_HINT]: buf.length },
    });
    writeFileSync(`${f}.br`, b);
    br += b.length;
  }
  unlinkSync(f);
  files++;
  raw += buf.length;
  gz += g.length;
}

if (files === 0) {
  console.error(`compress: nothing to compress in ${dist} (placeholder page?)`);
  process.exit(1);
}
console.log(
  `compress: ${files} files, ${kb(raw)} raw -> ${kb(gz)} gzip${brotli ? ` + ${kb(br)} brotli` : ''}; ` +
    `${kb(kept)} kept as is; embedded bundle ${kb(gz + br + kept)}`,
);
