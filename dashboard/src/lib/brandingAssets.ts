// Public branding accepts only small static PNGs. Header/chunk checks precede
// browser image decoding; the server remains the authoritative PNG decoder.
export const MAX_BRAND_BYTES = 64 * 1024;
const PREFIX = 'data:image/png;base64,';
const MAX_BASE64 = Math.ceil(MAX_BRAND_BYTES / 3) * 4;
const SIGNATURE = [137, 80, 78, 71, 13, 10, 26, 10];

export function brandingNameValid(name: string): boolean {
  return name.trim() === name && Array.from(name).length <= 80 && !/[\p{Cc}\p{Cf}\p{Cs}]/u.test(name);
}

export function brandingAccentValid(value: string): boolean {
  return value === '' || /^#[0-9a-f]{6}$/i.test(value);
}

/** Validate lengths before allocating or attempting to decode compressed pixels. */
export function pngDimensions(bytes: Uint8Array, maxDimension: number): { width: number; height: number } | null {
  if (bytes.length < 57 || bytes.length > MAX_BRAND_BYTES || SIGNATURE.some((n, i) => bytes[i] !== n)) return null;
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let offset = 8;
  let width = 0;
  let height = 0;
  let data = false;
  let chunks = 0;
  while (offset + 12 <= bytes.length && chunks++ < 4096) {
    const size = view.getUint32(offset);
    if (size > bytes.length - offset - 12) return null;
    const type = String.fromCharCode(...bytes.subarray(offset + 4, offset + 8));
    if (!/^[a-z]{4}$/i.test(type) || type === 'acTL' || type === 'fcTL' || type === 'fdAT') return null;
    if (offset === 8) {
      if (type !== 'IHDR' || size !== 13) return null;
      width = view.getUint32(offset + 8);
      height = view.getUint32(offset + 12);
      if (!width || !height || width > maxDimension || height > maxDimension) return null;
      const depth = bytes[offset + 16];
      const color = bytes[offset + 17];
      const depths: Record<number, readonly number[]> = { 0: [1, 2, 4, 8, 16], 2: [8, 16], 3: [1, 2, 4, 8], 4: [8, 16], 6: [8, 16] };
      if (!depths[color!]?.includes(depth!) || bytes[offset + 18] !== 0 || bytes[offset + 19] !== 0 || bytes[offset + 20]! > 1) return null;
    } else if (type === 'IHDR') return null;
    let crc = 0xffffffff;
    for (let i = offset + 4; i < offset + 8 + size; i++) {
      crc ^= bytes[i]!;
      for (let bit = 0; bit < 8; bit++) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
    }
    if (((crc ^ 0xffffffff) >>> 0) !== view.getUint32(offset + 8 + size)) return null;
    offset += size + 12;
    if (type === 'IDAT') data = true;
    if (type === 'IEND') return size === 0 && data && offset === bytes.length ? { width, height } : null;
  }
  return null;
}

export function brandingPNGValid(value: string, maxDimension = 1024): boolean {
  if (value === '') return true;
  if (!value.startsWith(PREFIX) || value.length > PREFIX.length + MAX_BASE64) return false;
  const encoded = value.slice(PREFIX.length);
  if (!encoded || !/^[a-z0-9+/]*={0,2}$/i.test(encoded) || encoded.length % 4 !== 0) return false;
  try {
    const raw = atob(encoded);
    if (raw.length > MAX_BRAND_BYTES || btoa(raw) !== encoded) return false;
    return pngDimensions(Uint8Array.from(raw, (c) => c.charCodeAt(0)), maxDimension) !== null;
  } catch {
    return false;
  }
}

export async function readBrandingPNG(file: File, maxDimension: number): Promise<string> {
  if (file.size > MAX_BRAND_BYTES || (file.type !== '' && file.type !== 'image/png')) throw new Error('Choose a static PNG of at most 64 KiB.');
  const bytes = new Uint8Array(await file.arrayBuffer());
  if (!pngDimensions(bytes, maxDimension)) throw new Error(`Choose a static PNG with dimensions between 1 and ${maxDimension} pixels, at most 64 KiB.`);
  // Input is already capped; avoid a large spread/call argument allocation.
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return PREFIX + btoa(binary);
}
