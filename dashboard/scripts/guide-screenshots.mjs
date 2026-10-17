// Takes the screenshots of the admin and user guide (docs/guide/).
//
//   cd dashboard && bun run dev:mock -- --port 5199 --strictPort &   # populated mock data
//   node scripts/guide-screenshots.mjs                         # dashboard shots
//
// The dashboard shots use the MSW mock of `bun run dev:mock` (src/mocks), so
// the data is the same on every run. The block page shots need a real node:
// start one with the Playwright helpers (e2e/support/kudurru.ts) and pass the
// block page URL in KUDURRU_GUIDE_BLOCKPAGE (for example http://127.0.0.1:8090).
//
// Environment:
//   KUDURRU_GUIDE_URL         mock dev server (default http://localhost:5199)
//   KUDURRU_GUIDE_BLOCKPAGE   block page of a real node (optional)
//   KUDURRU_GUIDE_CHROME      Chrome executable (default /usr/bin/google-chrome)
//   KUDURRU_GUIDE_ONLY        comma-separated shot names to retake
//   KUDURRU_GUIDE_GROUPS      comma-separated groups to run (sign in, monitor, lists, domains, groups,
//                             clients, policies, requests, block page, zones, upstreams, system, access,
//                             appearance, viewer, dhcp, dhcp-running)
//   KUDURRU_GUIDE_OUT         screenshot output directory (default ../docs/guide/images)
//
// Screenshots are PNG files in docs/guide/images/, 1280 pixels wide.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { browser } from './guide-shots/lib.mjs';

// One module per chapter of the guide, taken in file-name order, so that a
// chapter can be retaken alone with KUDURRU_GUIDE_ONLY.
const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), 'guide-shots');
for (const f of fs.readdirSync(dir).filter((x) => /^\d\d-.*\.mjs$/.test(x)).sort()) {
  await (await import(path.join(dir, f))).default();
}
await browser.close();
