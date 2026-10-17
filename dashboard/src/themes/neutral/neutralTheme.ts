// Astryx Neutral for the Appearance picker: the package theme unchanged
// except for the query status colours every kudurru theme carries
// (queryTokens.ts) and the fonts. Neutral names Figtree, which only loads
// from Google Fonts; the dashboard ships no font files and its CSP
// (font-src 'self') blocks font services, so this keeps Neutral's own
// fallback stack, which is what rendered anyway. Built with
// `bun run themes:build`.
import { defineTheme } from '@astryxdesign/core/theme';
import { neutralTheme as astryxNeutral } from '@astryxdesign/theme-neutral/built';
import { queryTokens } from './queryTokens';

const sans = { family: '-apple-system', fallbacks: 'BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif' };

export const neutralTheme = defineTheme({
  name: 'neutral',
  extends: astryxNeutral,
  localTokens: queryTokens,
  // Neutral's typography config with the fallback stack (scale configs
  // replace the base's, so the scale and weights are restated).
  typography: {
    scale: { base: 14, ratio: 1.2 },
    body: sans,
    heading: { ...sans, weights: { 3: 'bold', 4: 'bold' } },
    code: { family: 'ui-monospace', fallbacks: '"SF Mono", Monaco, Consolas, "Liberation Mono", "Courier New", monospace' },
  },
});
