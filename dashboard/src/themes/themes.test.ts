// The built themes of the Appearance picker (src/astryx-themes.ts): every
// theme carries Kudurru's query status colours, and the Kudurru theme's
// text, accent and border pairs meet WCAG AA in both modes.
import { describe, expect, it } from 'vitest';
import { themes } from '../astryx-themes';

type RGBA = [number, number, number, number];

/** A token value as [light, dark] colour strings. */
function modes(value: string | undefined): [string, string] {
  if (!value) throw new Error('token missing');
  const m = /^light-dark\(\s*([^,]+?)\s*,\s*([^)]+?)\s*\)$/.exec(value);
  return m ? [m[1]!, m[2]!] : [value, value];
}

function rgba(color: string): RGBA {
  const named: Record<string, string> = { white: '#ffffff', black: '#000000' };
  const hex = (named[color.toLowerCase()] ?? color).replace(/^#/, '');
  if (!/^[0-9a-f]{6}([0-9a-f]{2})?$/i.test(hex)) throw new Error(`not a hex colour: ${color}`);
  const n = (i: number) => parseInt(hex.slice(i, i + 2), 16);
  return [n(0), n(2), n(4), hex.length === 8 ? n(6) / 255 : 1];
}

function over(top: RGBA, bottom: RGBA): RGBA {
  const a = top[3];
  return [0, 1, 2].map((i) => top[i]! * a + bottom[i]! * (1 - a)).concat(1) as RGBA;
}

function luminance(c: RGBA): number {
  const ch = (v: number) => {
    const s = v / 255;
    return s <= 0.04045 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * ch(c[0]) + 0.7152 * ch(c[1]) + 0.0722 * ch(c[2]);
}

/** WCAG contrast of fg (composited over bg) against bg. */
function contrast(fg: string, bg: string): number {
  const b = rgba(bg);
  const [l1, l2] = [luminance(over(rgba(fg), b)), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (l1 + 0.05) / (l2 + 0.05);
}

const kudurru = themes.kudurru;
const token = (name: string) => modes(kudurru.tokens[name]);
const queryNames = Object.keys(kudurru.localTokens ?? {}).filter((k) => k.startsWith('--q-'));

describe('themes', () => {
  it('Kudurru is the default and declares the query status colours', () => {
    expect(queryNames).toEqual(
      expect.arrayContaining(['--q-permitted', '--q-cached', '--q-blocked', '--q-error', '--q-forwarded', '--q-authoritative', '--q-other', '--q-blocked-strong']),
    );
  });

  it.each(Object.keys(themes))('%s carries the same query status colours as Kudurru', (slug) => {
    const theme = themes[slug as keyof typeof themes];
    for (const name of queryNames) expect(theme.localTokens?.[name], `${slug} ${name}`).toBe(kudurru.localTokens?.[name]);
  });

  it.each([0, 1])('Kudurru text keeps 4.5:1 on its surfaces, hovered too (mode %i: light, dark)', (mode) => {
    const hex = (c: RGBA) => `#${c.slice(0, 3).map((v) => Math.round(v).toString(16).padStart(2, '0')).join('')}`;
    const texts = ['--color-text-primary', '--color-text-secondary', '--color-text-accent'];
    for (const s of ['--color-background-body', '--color-background-card', '--color-background-popover', '--color-background-muted']) {
      const bg = token(s)[mode]!;
      for (const t of texts) expect(contrast(token(t)[mode]!, bg), `${t} on ${s}`).toBeGreaterThanOrEqual(4.5);
      for (const i of ['--color-icon-primary', '--color-icon-secondary', '--color-icon-accent']) expect(contrast(token(i)[mode]!, bg), `${i} on ${s}`).toBeGreaterThanOrEqual(3);
    }
    // Rows and tiles: the translucent hover overlay over the page and the
    // cards (e2e/contrast.spec.ts measures the rendered pages).
    for (const s of ['--color-background-body', '--color-background-card']) {
      const hovered = hex(over(rgba(token('--color-overlay-hover')[mode]!), rgba(token(s)[mode]!)));
      for (const t of texts) expect(contrast(token(t)[mode]!, hovered), `${t} on hovered ${s}`).toBeGreaterThanOrEqual(4.5);
    }
    expect(contrast(token('--color-on-accent')[mode]!, token('--color-accent')[mode]!), 'on-accent on accent').toBeGreaterThanOrEqual(4.5);
    expect(contrast(token('--color-border-emphasized')[mode]!, token('--color-background-card')[mode]!), 'input border on card').toBeGreaterThanOrEqual(3);
  });

  it.each([0, 1])('the query status colours keep 4.5:1 on the Kudurru cards (mode %i)', (mode) => {
    const card = token('--color-background-card')[mode]!;
    for (const name of queryNames) {
      const fg = modes(kudurru.localTokens?.[name])[mode]!;
      expect(contrast(fg, card), name).toBeGreaterThanOrEqual(4.5);
    }
  });
});
