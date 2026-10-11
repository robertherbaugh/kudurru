// The logo of a blocked service or safe-search engine: the SVG the
// catalogue names (`icon`, an API path served from the binary, ux-plan C4,
// from homarr-labs/dashboard-icons) or, when there is none or it fails to
// load, a monogram on a colour picked from the ID. The name is always text
// next to it, so the image itself is decorative.
import { useState } from 'react';
import { useApi } from '../api/context';

export interface ServiceIconProps {
  id: string;
  name: string;
  icon?: string | undefined;
  /** Edge length in pixels (default 32). */
  size?: number;
}

/** One or two letters for the monogram: "YouTube" → "Y", "X (Twitter)" → "X", "9GAG" → "9G". */
export function monogram(name: string): string {
  const words = name.replace(/\(.*?\)/g, ' ').trim().split(/[\s+._-]+/).filter(Boolean);
  if (words.length >= 2) return ((words[0]?.[0] ?? '') + (words[1]?.[0] ?? '')).toUpperCase();
  const w = words[0] ?? name;
  return (/^\d/.test(w) ? w.slice(0, 2) : w.slice(0, 1)).toUpperCase() || '?';
}

/** A stable tone 0..7 for an ID (styles.css .k-monogram[data-tone]). */
export function toneOf(id: string): number {
  let h = 0;
  for (const ch of id) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
  return h % 8;
}

export function ServiceIcon({ id, name, icon, size = 32 }: ServiceIconProps) {
  const api = useApi();
  const [failed, setFailed] = useState(false);
  const style = { inlineSize: size, blockSize: size };
  if (icon && !failed) {
    return <img className="k-service-icon" src={api.catalogue.iconURL(icon)} alt="" aria-hidden="true" width={size} height={size} loading="lazy" decoding="async" onError={() => setFailed(true)} data-testid={`icon-${id}`} />;
  }
  return (
    <span className="k-monogram" data-tone={toneOf(id)} aria-hidden="true" style={{ ...style, fontSize: Math.round(size * 0.42) }} data-testid={`monogram-${id}`}>
      {monogram(name)}
    </span>
  );
}
