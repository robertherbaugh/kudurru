// Astryx's supported runtime theme API, limited to the accent family.
import { defineTheme, expandColorScale, resolveThemeTokens, type DefinedTheme, type TokenValue } from '@astryxdesign/core/theme';
import { themes, type ThemeSlug } from '../astryx-themes';
import { brandingAccentValid } from './brandingAssets';

function rgb(c: string): number[] {
  const hex = c.replace('#', '');
  if (!/^[0-9a-f]{6}([0-9a-f]{2})?$/i.test(hex)) throw new Error('Unresolved theme color');
  return [0, 2, 4].map((i) => parseInt(hex.slice(i, i + 2), 16));
}
function hex(ch: number[]): string { return '#' + ch.map((n) => Math.round(n).toString(16).padStart(2, '0')).join(''); }
function mix(fg: string, bg: string, amount: number): string { const a = rgb(fg), b = rgb(bg); return hex(a.map((v, i) => v * amount + b[i]! * (1 - amount))); }
export function colorContrast(a: string, b: string): number {
  const lum = (c: string) => rgb(c).map((v) => { const x = v / 255; return x <= 0.04045 ? x / 12.92 : ((x + 0.055) / 1.055) ** 2.4; }).reduce((s, v, i) => s + v * [0.2126, 0.7152, 0.0722][i]!, 0);
  const x = lum(a), y = lum(b); return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
}
function readable(seed: string, surfaces: string[], dark: boolean): string {
  for (let n = 0; n <= 100; n++) {
    const color = mix(dark ? '#ffffff' : '#000000', seed, n / 100);
    if (surfaces.every((s) => colorContrast(color, s) >= 4.5)) return color;
  }
  return dark ? '#ffffff' : '#000000';
}
export function brandingTheme(slug: ThemeSlug, accent: string, preview = false): DefinedTheme {
  const base = themes[slug];
  if (!accent || !brandingAccentValid(accent)) return base;
  try {
    const palette = expandColorScale({ accent });
    const tokens: Record<string, TokenValue> = {};
    for (const name of ['--color-accent', '--color-on-accent', '--color-accent-muted']) tokens[name] = palette[name]!;
    const pairs = (['light', 'dark'] as const).map((mode) => {
      const t = resolveThemeTokens(base, { mode });
      const surfaces = ['--color-background-body', '--color-background-card', '--color-background-popover', '--color-background-muted'].map((s) => t[s]!);
      for (const s of [...surfaces]) {
        // Both hover and selection overlays are part of the contrast envelope.
        for (const overlay of ['--color-overlay-hover', '--color-overlay-selected']) {
          const v = t[overlay];
          if (v && /^#[0-9a-f]{8}$/i.test(v)) surfaces.push(mix(v, s, parseInt(v.slice(7), 16) / 255));
        }
      }
      const generated = palette['--color-accent']!;
      const halves = /^light-dark\(([^,]+),\s*([^)]+)\)$/.exec(generated);
      return readable(halves ? halves[mode === 'light' ? 1 : 2]! : accent, surfaces, mode === 'dark');
    }) as [string, string];
    tokens['--color-text-accent'] = pairs;
    tokens['--color-icon-accent'] = pairs;
    tokens['--shadow-inset-hover'] = 'inset 0 0 0 1px color-mix(in srgb, var(--color-accent) 25%, transparent)';
    tokens['--shadow-inset-selected'] = 'inset 0 0 0 1px var(--color-text-accent)';
    return defineTheme({ name: `organization-${preview ? 'preview-' : ''}${slug}`, extends: base, tokens });
  } catch { return base; }
}
