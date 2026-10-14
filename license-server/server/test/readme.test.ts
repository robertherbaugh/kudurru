// The README's configuration table (README.md, "Configuration") names every
// variable of CONFIG_VARS, the server's one list of settings, and nothing
// the server does not read.
import { expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { CONFIG_VARS } from '../config';

test('README.md documents every configuration variable, and only those', () => {
  const readme = readFileSync(join(import.meta.dir, '..', '..', 'README.md'), 'utf8');
  const section = readme.slice(readme.indexOf('## Configuration'), readme.indexOf('## Deploying'));
  const documented = [...section.matchAll(/^\| `([A-Z][A-Z0-9_]*)` \|/gm)].map((m) => m[1]);
  expect(documented).toEqual(CONFIG_VARS.map((v) => v.name));
});
