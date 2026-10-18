// The query status colours, kudurru's own and the same in every theme
// (D131). The values are stops of the Kudurru palette
// (src/themes/kudurru/kudurruTheme.ts); a theme module may only import
// files from its own folder, so each theme keeps a copy and
// themes.test.ts checks that all of them match Kudurru.
import type { TokenValue } from '@astryxdesign/core/theme';

export const queryTokens: Record<string, TokenValue> = {
  '--q-permitted': ['#1d7a35', '#6fd08a'],
  '--q-cached': ['#2459c7', '#86aeff'],
  '--q-blocked': ['#c42b3a', '#ff8a8a'],
  '--q-error': ['#8a5a00', '#f2c14e'],
  '--q-forwarded': ['#0b7566', '#5ccfbf'],
  '--q-authoritative': ['#6e45c8', '#b79cff'],
  '--q-other': ['#5f6b76', '#9aa5b1'],
  '--q-permitted-strong': ['#05631a', '#5fe07a'],
  '--q-cached-strong': ['#0b3fa8', '#8ab4ff'],
  '--q-blocked-strong': ['#a3001c', '#ff7a8a'],
  '--q-error-strong': ['#7a5600', '#ffd54a'],
};
