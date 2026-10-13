// The container environment (src/env.ts) against the server's own list of
// settings (server/config.ts CONFIG_VARS): every server variable is either
// forwarded or excluded on purpose, and nothing forwarded is a typo.

import { describe, expect, test } from 'bun:test';
import { AWAITING_SERVER, envDigest, NOT_FORWARDED, SERVER_ENV, serverEnv } from '../src/env';

// A computed path, so tsc does not type-check the server's sources with
// this package's settings; bun imports the real module.
const serverConfig = new URL('../../server/config.ts', import.meta.url).pathname;
const { CONFIG_VARS } = (await import(serverConfig)) as { CONFIG_VARS: readonly { name: string }[] };
const serverNames = CONFIG_VARS.map((v) => v.name);
const forwarded: readonly string[] = SERVER_ENV;

describe('SERVER_ENV', () => {
  test('covers every server setting, or says why not', () => {
    const missing = serverNames.filter((n) => !forwarded.includes(n) && !(n in NOT_FORWARDED));
    expect(missing).toEqual([]);
  });

  test('forwards only names the server reads (or is about to)', () => {
    const unknown = forwarded.filter((n) => !serverNames.includes(n) && !AWAITING_SERVER.includes(n));
    expect(unknown).toEqual([]);
  });

  test('never forwards a name it also excludes', () => {
    expect(forwarded.filter((n) => n in NOT_FORWARDED)).toEqual([]);
  });
});

describe('serverEnv', () => {
  test('copies set string values of known names only', () => {
    const env = {
      PUBLIC_URL: 'https://licenses.kudurru.io',
      DATABASE_URL: 'postgres://u:p@db.example.org:5432/portal',
      OIDC_SCOPES: '',
      PORT: '9999',
      HOST: '127.0.0.1',
      UNRELATED: 'x',
      PORTAL: { idFromName: () => null },
    };
    expect(serverEnv(env)).toEqual({
      PUBLIC_URL: 'https://licenses.kudurru.io',
      DATABASE_URL: 'postgres://u:p@db.example.org:5432/portal',
    });
  });
});

describe('envDigest', () => {
  test('ignores key order and notices any changed value', async () => {
    const a = await envDigest({ A: '1', B: '2' });
    expect(await envDigest({ B: '2', A: '1' })).toBe(a);
    expect(await envDigest({ A: '1', B: '3' })).not.toBe(a);
    expect(await envDigest({ A: '1' })).not.toBe(a);
    expect(a).toMatch(/^[0-9a-f]{64}$/);
  });
});
