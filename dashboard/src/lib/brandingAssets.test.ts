import { deflateSync } from 'node:zlib';
import { describe, expect, it } from 'vitest';
import { brandingAccentValid, brandingNameValid, brandingPNGValid, MAX_BRAND_BYTES, pngDimensions, readBrandingPNG } from './brandingAssets';
function chunk(type: string, data: Uint8Array) {
  const b = Buffer.alloc(data.length + 12); b.writeUInt32BE(data.length); b.write(type, 4); b.set(data, 8);
  let crc = 0xffffffff;
  for (const byte of b.subarray(4, -4)) { crc ^= byte; for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ (crc & 1 ? 0xedb88320 : 0); }
  b.writeUInt32BE((crc ^ 0xffffffff) >>> 0, b.length - 4); return b;
}
function png(width = 1, height = 1, extra?: string) {
  const ihdr = Buffer.alloc(13); ihdr.writeUInt32BE(width); ihdr.writeUInt32BE(height, 4); ihdr[8] = 8; ihdr[9] = 6;
  return Buffer.concat([Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]), chunk('IHDR', ihdr), ...(extra ? [chunk(extra, Buffer.alloc(8))] : []), chunk('IDAT', deflateSync(Buffer.from([0, 0, 0, 0, 255]))), chunk('IEND', Buffer.alloc(0))]);
}
const url = (b: Uint8Array) => `data:image/png;base64,${Buffer.from(b).toString('base64')}`;
describe('bounded branding assets', () => {
  it('accepts static canonical PNG and empty resets', () => { expect(brandingPNGValid(url(png()))).toBe(true); expect(brandingPNGValid('')).toBe(true); expect(pngDimensions(png(), 256)).toEqual({ width: 1, height: 1 }); });
  it.each(['acTL', 'fcTL', 'fdAT'])('rejects animated PNG chunk %s', (type) => { expect(brandingPNGValid(url(png(1, 1, type)))).toBe(false); });
  it('checks dimensions before pixel decode, including the smaller favicon bound', () => { expect(brandingPNGValid(url(png(257)), 256)).toBe(false); expect(brandingPNGValid(url(png(1025)))).toBe(false); expect(brandingPNGValid(url(png(0)))).toBe(false); });
  it('rejects corruption, truncation, trailing bytes and oversized data', () => { const b = png(); b[25] = b[25]! ^ 1; for (const bad of [b, png().subarray(0, -1), Buffer.concat([png(), Buffer.from([0])]), Buffer.alloc(MAX_BRAND_BYTES + 1)]) expect(brandingPNGValid(url(bad))).toBe(false); });
  it.each(['https://example.com/logo.png', 'data:image/svg+xml;base64,PHN2Zz4=', 'data:image/png;base64,_w==', 'data:image/png;base64, AAAA'])('rejects remote/noncanonical image %s', (bad) => { expect(brandingPNGValid(bad)).toBe(false); });
  it('bounds Unicode names by code points and rejects controls/surrogates', () => { expect(brandingNameValid('🦊'.repeat(80))).toBe(true); expect(brandingNameValid('🦊'.repeat(81))).toBe(false); expect(brandingNameValid('a\nb')).toBe(false); expect(brandingNameValid('\ud800')).toBe(false); });
  it('allows only complete six-digit accent seeds', () => { expect(brandingAccentValid('')).toBe(true); expect(brandingAccentValid('#a1B2c3')).toBe(true); for (const v of ['#abc', 'red', '#123456; color:red', 'url(x)']) expect(brandingAccentValid(v)).toBe(false); });
  it('rejects huge/incorrect-type files before reading', async () => { let reads = 0; const file = { size: MAX_BRAND_BYTES + 1, type: 'image/png', arrayBuffer: () => { reads++; return Promise.resolve(new ArrayBuffer(0)); } } as unknown as File; await expect(readBrandingPNG(file, 1024)).rejects.toThrow(); expect(reads).toBe(0); await expect(readBrandingPNG({ ...file, size: 1, type: 'image/svg+xml' }, 1024)).rejects.toThrow(); expect(reads).toBe(0); });
  it('reads a bounded file without external requests', async () => { const b = png(); const file = { size: b.length, type: 'image/png', arrayBuffer: () => Promise.resolve(Uint8Array.from(b).buffer) } as File; expect(await readBrandingPNG(file, 1024)).toBe(url(b)); });
});
