// Global setup of the Playwright suite: one `kudurru serve` with the
// embedded dashboard for every test, a stub upstream, a signed-in admin
// session (storage state) and real DNS traffic over UDP so the stats,
// query log and clients pages have data. The returned function is the
// global teardown. A stub list publisher serves hosts lists for url
// sources (KUDURRU_E2E_LISTS).
import { request, type FullConfig } from '@playwright/test';
import fs from 'node:fs';
import path from 'node:path';
import { query, startUpstream, TYPE_AAAA, UPSTREAM_A } from './support/dns';
import { startListServer } from './support/lists';
import { ADMIN_USER, BLOCKLIST, locateBinary, sleep, startServer, WEB_DIR, ZONE } from './support/kudurru';

export const STORAGE_STATE = path.join(WEB_DIR, 'e2e', '.auth', 'admin.json');

export default async function globalSetup(_config: FullConfig): Promise<() => Promise<void>> {
  const bin = locateBinary();
  const upstream = await startUpstream();
  // The block-list publisher url sources fetch from (allowlist.spec.ts).
  const lists = await startListServer();
  const adminPassword = `e2e-${Math.random().toString(36).slice(2)}-password`;
  const server = await startServer(bin, { upstream: upstream.address, adminPassword });
  const teardown = async () => {
    await server.stop();
    await upstream.close();
    await lists.close();
  };
  try {
    // The tests must run against the real bundle, not the placeholder.
    const api = await request.newContext({ baseURL: server.url, ignoreHTTPSErrors: true });
    const index = await (await api.get('/')).text();
    if (index.includes('kudurru:placeholder')) {
      throw new Error(`${bin} embeds the placeholder dashboard: run \`make dashboard-build\` before building it (or \`make dashboard-e2e\`)`);
    }

    const login = await api.post('/api/v1/auth/login', { data: { username: ADMIN_USER, password: adminPassword } });
    if (!login.ok()) throw new Error(`login failed: ${login.status()} ${await login.text()}`);
    fs.mkdirSync(path.dirname(STORAGE_STATE), { recursive: true });
    await api.storageState({ path: STORAGE_STATE });

    // Real traffic from 127.0.0.1: local zone, blocked and recursive names.
    const local = await query(server.dns, ZONE.host);
    if (!local.a.includes(ZONE.hostA)) throw new Error(`${ZONE.host}: want A ${ZONE.hostA}, got ${JSON.stringify(local)}`);
    const recursive = await query(server.dns, 'www.example.com');
    if (!recursive.a.includes(UPSTREAM_A)) throw new Error(`www.example.com: want A ${UPSTREAM_A} from the stub upstream, got ${JSON.stringify(recursive)}`);
    const names = [ZONE.host, `www.${ZONE.object}`, BLOCKLIST.blocked, `cdn.${BLOCKLIST.blocked}`, BLOCKLIST.alsoBlocked, 'www.example.com', 'example.org'];
    let sent = 2;
    for (let round = 0; round < 3; round++) {
      for (const n of names) {
        await query(server.dns, n);
        sent++;
      }
    }
    await query(server.dns, 'example.org', TYPE_AAAA);
    sent++;

    // The query log writes in batches: wait until the summary counts the traffic.
    const deadline = Date.now() + 20_000;
    for (;;) {
      const r = await api.get('/api/v1/stats/summary?window=24h');
      const body = r.ok() ? ((await r.json()) as { queries?: { total?: number } }) : {};
      if ((body.queries?.total ?? 0) >= sent) break;
      if (Date.now() > deadline) throw new Error(`stats/summary counts ${body.queries?.total ?? 'nothing'} of ${sent} queries after 20 s`);
      await sleep(250);
    }
    await api.dispose();

    process.env.KUDURRU_E2E_URL = server.url;
    process.env.KUDURRU_E2E_DNS = server.dns;
    process.env.KUDURRU_E2E_BLOCKPAGE = server.blockPage;
    process.env.KUDURRU_E2E_NODE = server.node;
    process.env.KUDURRU_E2E_UPSTREAM = upstream.address;
    process.env.KUDURRU_E2E_LISTS = lists.url;
    process.env.KUDURRU_E2E_ADMIN_PASSWORD = adminPassword;
    process.env.KUDURRU_E2E_BIN_PATH = bin;
    console.log(`e2e: kudurru ${server.node} at ${server.url} (dns ${server.dns}, data ${server.dir})`);
  } catch (err) {
    console.error(server.log());
    await teardown();
    throw err;
  }
  return teardown;
}
