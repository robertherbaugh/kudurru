// The Kudurru theme, the dashboard default (D131): Astryx Neutral with the
// docs/brand palette. Paper and Ink surfaces, Stone dividers, the Clay
// accent; Clay as text is a darker stop on light surfaces and a lighter one
// on Ink so it keeps 4.5:1. Every colour is a stop of the palette generated
// from the brand (tokens/kudurru.palette.ts, regenerate with
// `bun run astryx -- theme palette generate src/themes/kudurru/palette.config.json
// --out src/themes/kudurru/tokens/kudurru.palette.ts --overwrite`).
//
// Hover and pressed are translucent Ink (light) or Paper (dark) overlays
// drawn over whatever background an element has, so text on a hovered row
// or tile keeps its contrast (e2e/contrast.spec.ts).
//
// The query status colours (--q-*) are kudurru's own and separate from the
// brand (docs/brand/README.md): light/dark pairs that keep 4.5:1 as text on
// the cards, plus the stronger -strong variants the high-contrast setting
// switches to (styles.css). The Neutral, Stone and Y2K themes in
// src/themes carry the same values (themes.test.ts checks it).
//
// Built with `bun run themes:build`; the app imports the built kudurru.js
// and kudurru.css through src/astryx-themes.ts.
import { defineTheme, type TokenValue } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';
import { palette, white } from './tokens/kudurru.palette';

const { ink, sand, clay, green, blue, red, amber, teal, purple, slate } = palette;
const paper = sand.dark[95];
const inkColor = ink.light[12.5];
const withAlpha = (color: string, alpha: string) => `${color}${alpha}`;

const queryTokens: Record<string, TokenValue> = {
  '--q-permitted': [green.light[45], green.dark[75]],
  '--q-cached': [blue.light[40], blue.dark[70]],
  '--q-blocked': [red.light[45], red.dark[70]],
  '--q-error': [amber.light[42.5], amber.dark[80]],
  '--q-forwarded': [teal.light[45], teal.dark[77.5]],
  '--q-authoritative': [purple.light[40], purple.dark[70]],
  '--q-other': [slate.light[45], slate.dark[67.5]],
  '--q-permitted-strong': [green.light[35], green.dark[80]],
  '--q-cached-strong': [blue.light[30], blue.dark[72.5]],
  '--q-blocked-strong': [red.light[32.5], red.dark[67.5]],
  '--q-error-strong': [amber.light[40], amber.dark[87.5]],
};

// Fonts: the platform's UI and monospace faces. The brand recommends Inter
// and JetBrains Mono, but the dashboard ships no font files and the CSP
// (font-src 'self') rules out font services, so the theme names only what
// renders.
const sans = { family: 'ui-sans-serif', fallbacks: 'system-ui, -apple-system, "Segoe UI", Roboto, sans-serif' };

export const kudurruTheme = defineTheme({
  name: 'kudurru',
  extends: neutralTheme,
  localTokens: queryTokens,

  // Neutral's scale and heading weights (scale configs replace the base's).
  typography: {
    scale: { base: 14, ratio: 1.2 },
    body: sans,
    heading: { ...sans, weights: { 3: 'bold', 4: 'bold' } },
    code: { family: 'ui-monospace', fallbacks: '"SF Mono", Menlo, Consolas, "Liberation Mono", monospace' },
  },

  tokens: {
    // Surfaces: Paper and white cards in light mode, Ink and its lighter
    // stops in dark mode.
    '--color-background-body': [sand.light[95], ink.dark[12.5]],
    '--color-background-surface': [sand.light[95], ink.dark[12.5]],
    '--color-background-card': [white, ink.dark[15]],
    '--color-background-popover': [white, ink.dark[17.5]],
    '--color-background-muted': [sand.light[92.5], ink.dark[20]],
    '--color-background-gray': [sand.light[90], ink.dark[22.5]],
    '--color-syntax-background': [white, ink.dark[10]],
    '--color-skeleton': [sand.light[92.5], ink.dark[22.5]],

    // Overlays: modal scrim, neutral fill, hover and pressed tints.
    '--color-overlay': [withAlpha(inkColor, '80'), withAlpha(ink.dark[5], 'CC')],
    '--color-neutral': [withAlpha(inkColor, '0F'), withAlpha(paper, '1A')],
    '--color-overlay-hover': [withAlpha(inkColor, '0D'), withAlpha(paper, '12')],
    '--color-overlay-pressed': [withAlpha(inkColor, '1A'), withAlpha(paper, '21')],
    '--color-tint-hover': [inkColor, paper],

    // Accent: Clay fills the primary actions (white on Clay 4.6:1).
    '--color-accent': [clay.light[50], clay.dark[50]],
    '--color-accent-muted': [clay.light[92.5], withAlpha(clay.dark[50], '40')],
    '--color-on-accent': white,
    '--shadow-inset-hover': `inset 0px 0px 0px 2px ${withAlpha(clay.light[50], '4D')}`,
    '--shadow-inset-selected': `inset 0px 0px 0px 2px ${withAlpha(clay.light[50], '80')}`,

    // Text and icons: Ink on Paper, Paper on Ink; secondary text 4.5:1 on
    // the muted surfaces.
    '--color-text-primary': [inkColor, paper],
    '--color-text-secondary': [ink.light[37.5], sand.light[70]],
    '--color-text-disabled': [ink.light[60], ink.dark[47.5]],
    '--color-text-accent': [clay.light[40], clay.dark[67.5]],
    '--color-icon-primary': [inkColor, paper],
    '--color-icon-secondary': [ink.light[42.5], sand.light[65]],
    '--color-icon-disabled': [ink.light[60], ink.dark[47.5]],
    '--color-icon-accent': [clay.light[40], clay.dark[67.5]],

    // Borders: Stone dividers; input borders keep 3:1 against their surface.
    '--color-border': [sand.light[85], withAlpha(paper, '1F')],
    '--color-border-emphasized': [sand.light[60], ink.dark[50]],
  },
});
