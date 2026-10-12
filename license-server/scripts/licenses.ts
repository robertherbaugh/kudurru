// Writes dist/ui/third-party-licenses.txt, the notices of the code bundled
// into the UI. A placeholder from the Phase 0 tooling so `bun run build`
// runs end to end: stream D replaces it with the walk over the production
// dependencies (web/scripts/licenses.ts, with the copyleft deny-list) for
// the UI bundle and the server.
//
//   bun scripts/licenses.ts [dist/ui]
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const dist = process.argv[2] ?? join('dist', 'ui');
mkdirSync(dist, { recursive: true });
writeFileSync(
  join(dist, 'third-party-licenses.txt'),
  'Third-party software in the kudurru license portal UI bundle.\n\n(The list is not generated yet.)\n',
);
console.log(`wrote ${join(dist, 'third-party-licenses.txt')}`);
