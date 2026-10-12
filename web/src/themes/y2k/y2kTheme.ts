// Astryx Y2K (the fun theme) for the Appearance picker: the package theme
// unchanged except for the query status colours every kudurru theme
// carries (queryTokens.ts) and the fonts. Y2K names Poppins, Crimson Text
// and JetBrains Mono, which only load from Google Fonts; the dashboard
// ships no font files and its CSP (font-src 'self') blocks font services,
// so this keeps Y2K's own fallback stacks, which is what rendered anyway.
// Built with `bun run themes:build`.
import { defineTheme } from '@astryxdesign/core/theme';
import { y2kTheme as astryxY2k } from '@astryxdesign/theme-y2k/built';
import { queryTokens } from './queryTokens';

const sans = { family: '-apple-system', fallbacks: 'BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif' };
const display = { fontFamily: 'Georgia, "Times New Roman", Times, serif' };

export const y2kTheme = defineTheme({
  name: 'y2k',
  extends: astryxY2k,
  localTokens: queryTokens,
  // Y2K's typography config with the fallback stacks (scale configs
  // replace the base's, so the scale and Y2K's own supporting size are
  // restated).
  typography: {
    scale: { base: 16, ratio: 1.25 },
    body: sans,
    heading: sans,
    code: { family: 'SF Mono', fallbacks: 'Monaco, Consolas, monospace' },
  },
  tokens: {
    '--text-supporting-size': '12px',
  },
  // The display sizes keep their serif, minus Crimson Text.
  components: {
    text: {
      'type:display-1': display,
      'type:display-2': display,
      'type:display-3': display,
    },
  },
});
