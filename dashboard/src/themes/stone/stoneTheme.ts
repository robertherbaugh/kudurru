// Astryx Stone for the Appearance picker: the package theme unchanged
// except for the query status colours every kudurru theme carries
// (queryTokens.ts) and the fonts. Stone names Figtree, Montserrat and
// JetBrains Mono, which only load from Google Fonts; the dashboard ships
// no font files and its CSP (font-src 'self') blocks font services, so
// this keeps Stone's own fallback stacks, which is what rendered anyway.
// Built with `bun run themes:build`.
import { defineTheme } from '@astryxdesign/core/theme';
import { stoneTheme as astryxStone } from '@astryxdesign/theme-stone/built';
import { queryTokens } from './queryTokens';

const sans = { family: '-apple-system', fallbacks: 'BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif' };

export const stoneTheme = defineTheme({
  name: 'stone',
  extends: astryxStone,
  localTokens: queryTokens,
  // Stone's typography config with the fallback stacks (scale configs
  // replace the base's, so the scale, the weights and Stone's own
  // supporting size are restated).
  typography: {
    scale: { base: 14, ratio: 1.25 },
    body: sans,
    heading: { ...sans, weights: { 3: 'bold', 4: 'bold' } },
    code: { family: 'SF Mono', fallbacks: 'Monaco, Consolas, monospace' },
  },
  tokens: {
    '--text-supporting-size': '12px',
  },
});
