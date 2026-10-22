import { describe, expect, it, vi } from 'vitest';
import type * as ThemeModule from '@astryxdesign/core/theme';
import * as themeAPI from '@astryxdesign/core/theme';
import { getRegisteredThemes, resolveThemeTokens } from '@astryxdesign/core/theme';
import { themes, type ThemeSlug } from '../astryx-themes';
import { brandingTheme, colorContrast } from './brandingTheme';
vi.mock('@astryxdesign/core/theme', async (original) => { const actual = await original<typeof ThemeModule>(); return { ...actual, resolveThemeTokens: vi.fn(actual.resolveThemeTokens) }; });
describe('Enterprise accent child themes', () => {
  it.each(Object.keys(themes) as ThemeSlug[])('%s preserves surfaces, typography, semantic and query colors in either mode', (slug) => {
    const base = themes[slug];
    for (const seed of ['#ffffff', '#000000', '#ff00ff', '#0064E0', '#ffff00']) {
      const brand = brandingTheme(slug, seed);
      expect(brand.localTokens).toEqual(base.localTokens);
      for (const mode of ['light', 'dark'] as const) {
        const before = resolveThemeTokens(base, { mode }), after = resolveThemeTokens(brand, { mode });
        expect(colorContrast(after['--color-accent']!, after['--color-on-accent']!)).toBeGreaterThanOrEqual(4.5);
        for (const s of ['--color-background-body', '--color-background-card', '--color-background-popover', '--color-background-muted']) {
          expect(after[s]).toBe(before[s]);
          expect(colorContrast(after['--color-text-accent']!, after[s]!)).toBeGreaterThanOrEqual(4.5);
          expect(colorContrast(after['--color-icon-accent']!, after[s]!)).toBeGreaterThanOrEqual(3);
          expect(colorContrast(after['--focus-outline-color']!, after[s]!)).toBeGreaterThanOrEqual(3);
        }
        for (const name of Object.keys(before).filter((n) => /^(--q-|--font-|--color-(positive|negative|warning|danger|success))/.test(n))) expect(after[name], name).toBe(before[name]);
      }
    }
  });
  it('uses a bounded set of registered names across arbitrary edits', () => { for (let i = 0; i < 40; i++) brandingTheme('kudurru', '#' + i.toString(16).padStart(6, '0')); const names = Array.from(getRegisteredThemes().keys()).filter((name) => name.startsWith('organization-')); expect(names).toContain('organization-kudurru'); expect(names.length).toBeLessThanOrEqual(8); });
  it('fails softly if a selected theme contains an unknown color representation', () => { const spy = vi.spyOn(themeAPI, 'resolveThemeTokens').mockReturnValueOnce({ '--color-background-body': 'unknown-color()' }); expect(brandingTheme('stone', '#0064e0')).toBe(themes.stone); spy.mockRestore(); });
  it('returns the original selected theme on empty/invalid accent', () => { expect(brandingTheme('stone', '')).toBe(themes.stone); expect(brandingTheme('y2k', 'red')).toBe(themes.y2k); });
});
