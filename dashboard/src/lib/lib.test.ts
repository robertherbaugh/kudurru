import { describe, expect, it, vi } from 'vitest';
import { ApiClient, ApiError, encodeQuery } from '../api/client';
import type { BlockList } from '../api/types';
import { apiBaseFor, joinBase, stripBase } from './basePath';
import { formatCount, formatCountdown, formatVersion, parseDuration, toGoDuration } from './format';
import { listHealth } from './listHealth';
import { ownership, readOnlyReason, withMeta } from './managedBy';
import { isApiURL, isAppRoute, matchPath } from './router';
import { envVarName, getPath, lockFor, mergePatchFor, parseValue } from './settings';
import { settingKey } from '../test/utils';

describe('base path', () => {
  it.each([
    ['/', '/api/v1'],
    ['/kudurru/', '/kudurru/api/v1'],
  ])('apiBaseFor(%s) = %s', (b, want) => expect(apiBaseFor(b)).toBe(want));

  it('joins and strips routes', () => {
    expect(joinBase('/kudurru/', '/queries?x=1')).toBe('/kudurru/queries?x=1');
    expect(stripBase('/kudurru/', '/kudurru/zones/a')).toBe('/zones/a');
    expect(stripBase('/kudurru/', '/kudurru')).toBe('/');
  });
});

describe('router', () => {
  it.each([
    ['/queries', true],
    ['//evil.example/x', false],
    ['https://example.com', false],
    ['/api/v1/zones/a/export', false],
    ['/kudurru/api/v1/auth/oidc/dex/start?next=%2F', false],
    ['/zones/api-zone', true],
  ])('isAppRoute(%s) = %s', (to, want) => expect(isAppRoute(to)).toBe(want));

  it('recognises API URLs under any prefix', () => {
    expect(isApiURL('/api/v1')).toBe(true);
    expect(isApiURL('/a/b/api/v2/x')).toBe(true);
    expect(isApiURL('/queries?domain=api/v1')).toBe(false);
  });

  it('matches parameters', () => {
    expect(matchPath('/zones/:zone', '/zones/home%2Darpa')).toEqual({ zone: 'home-arpa' });
    expect(matchPath('/zones/:zone', '/zones')).toBeNull();
    expect(matchPath('/', '/')).toEqual({});
  });
});

describe('format', () => {
  it.each([
    ['0.1.0', 'v0.1.0'],
    ['v0.1.0', 'v0.1.0'],
    [' v0.1.0-RC.2 ', 'v0.1.0-RC.2'],
    ['v0.1.0-rc.1+build.123', 'v0.1.0-rc.1'],
    ['v0.1.0-rc.1-12-gdeadbee-dirty', 'v0.1.0-rc.1'],
    ['v0.1.0-12-gdeadbee', 'v0.1.0'],
    ['v0.1.0-dirty', 'v0.1.0'],
    ['1.2.3-beta.2+linux.amd64', 'v1.2.3-beta.2'],
    ['dev', 'dev'],
    ['devel-055b3ae', 'dev'],
    [undefined, '–'],
    ['', '–'],
  ])('shows release version %s as %s', (version, label) => expect(formatVersion(version)).toBe(label));

  it.each([
    [999, '999'],
    [9_999, '9,999'],
    [12_900, '12.9K'],
    [4_200_000, '4.2M'],
  ])('formatCount(%s) = %s', (n, want) => expect(formatCount(n)).toBe(want));

  it.each([
    ['10m', 600],
    ['1h30m', 5400],
    ['1500ms', 1.5],
    ['0', 0],
    ['10 minutes', Number.NaN],
    ['', Number.NaN],
  ])('parseDuration(%s) = %s', (s, want) => expect(parseDuration(s)).toBe(want));

  it('formats countdowns and Go durations', () => {
    expect(formatCountdown(65)).toBe('1:05');
    expect(formatCountdown(3723)).toBe('1:02:03');
    expect(toGoDuration(5400)).toBe('1h30m');
  });
});

describe('settings metadata', () => {
  const key = settingKey({ path: 'querylog.retention.raw', type: 'duration', scope: 'cluster', default: '24h' });

  it('derives env var names and merge patches', () => {
    expect(envVarName('querylog.retention.raw')).toBe('KUDURRU_QUERYLOG__RETENTION__RAW');
    expect(mergePatchFor('a.b.c', 1)).toEqual({ a: { b: { c: 1 } } });
    expect(getPath({ a: { b: 2 } }, 'a.b')).toBe(2);
  });

  it('locks host-only before env-locked before permissions', () => {
    expect(lockFor({ ...key, hostOnly: true, envLocked: true }, true)).toMatchObject({ locked: true, reason: expect.stringMatching(/^Host-only/) as string });
    expect(lockFor({ ...key, envLocked: true }, true)).toMatchObject({ locked: true, reason: expect.stringMatching(/environment variable/) as string });
    expect(lockFor(key, false)).toMatchObject({ locked: true });
    expect(lockFor(key, true)).toEqual({ locked: false });
  });

  it.each([
    ['int', '42', 42],
    ['bool', 'false', false],
    ['duration', '90s', '90s'],
    ['string[]', 'a\nb, c', ['a', 'b', 'c']],
    ['map', '{"x":1}', { x: 1 }],
  ] as const)('parses %s', (type, text, want) => expect(parseValue({ ...key, type }, text)).toEqual(want));

  it.each([
    ['int', '4.2', 'Must be a whole number'],
    ['duration', '5 minutes', 'A Go duration such as 30s, 10m or 1h30m'],
    ['list', '{}', 'A JSON array'],
  ] as const)('rejects bad %s', (type, text, msg) => expect(() => parseValue({ ...key, type }, text)).toThrow(msg));
});

describe('ownership', () => {
  it.each([
    ['', 'none', true],
    ['api', 'api', true],
    ['bootstrap', 'bootstrap', true],
    ['file:objects.d/zones.yaml', 'file', false],
    ['kubernetes', 'kubernetes', false],
    ['integration:external-dns', 'integration', false],
  ] as const)('%s → %s (editable %s)', (owner, kind, editable) => {
    expect(ownership(owner)).toMatchObject({ kind, editable });
  });

  it('explains read-only objects and sets comment/enabled annotations', () => {
    expect(readOnlyReason(ownership('integration:external-dns'))).toBe('Managed by the external-dns integration.');
    const list = { apiVersion: 'kudurru.io/v1alpha1', kind: 'BlockList', metadata: { name: 'x', annotations: { a: '1' } }, spec: { sources: [] } } as BlockList;
    expect(withMeta(list, ' hi ', false).metadata.annotations).toEqual({ a: '1', 'kudurru.io/comment': 'hi', 'kudurru.io/enabled': 'false' });
    expect(withMeta(list, '', true).metadata.annotations).toEqual({ a: '1' });
  });
});

describe('listHealth', () => {
  const base = { apiVersion: 'kudurru.io/v1alpha1', kind: 'BlockList', metadata: { name: 'l' }, spec: { sources: [] } } as const;
  it.each([
    [undefined, 'pending'],
    [{ entries: 5, conditions: [{ type: 'Ready', status: 'True' }] }, 'ok'],
    [{ entries: 5, sources: [{ name: 's', error: 'timeout' }] }, 'cached'],
    [{ entries: 0, sources: [{ name: 's', error: 'timeout' }], conditions: [{ type: 'Ready', status: 'True' }] }, 'unavailable'],
    [{ entries: 1, conditions: [{ type: 'Ready', status: 'False', message: 'bad line' }] }, 'invalid'],
    [{ entries: 1, conditions: [{ type: 'Ready', status: 'True', reason: 'Unchanged' }] }, 'unchanged'],
  ] as const)('%j → %s', (status, state) => {
    expect(listHealth({ ...base, status } as unknown as BlockList).state).toBe(state);
  });
});

describe('ApiClient', () => {
  it('sends the CSRF header on writes only and parses the error envelope', async () => {
    const calls: { url: string; init: RequestInit }[] = [];
    const fetchMock = vi.fn((url: string, init: RequestInit) => {
      calls.push({ url, init });
      if (init.method === 'PUT') {
        return Promise.resolve(
          new Response(JSON.stringify({ error: { key: 'managed_by', message: 'zone a is managed elsewhere', hint: 'file:objects.d/zones.yaml' } }), {
            status: 409,
            headers: { 'Content-Type': 'application/json' },
          }),
        );
      }
      return Promise.resolve(new Response(JSON.stringify({ ok: true }), { headers: { 'Content-Type': 'application/json' } }));
    });
    const c = new ApiClient({ baseUrl: '/kudurru/api/v1/', csrf: () => 'tok', fetch: fetchMock as unknown as typeof fetch });
    await c.get('/zones', { limit: 5, q: '' });
    expect(calls[0]?.url).toBe('/kudurru/api/v1/zones?limit=5');
    expect((calls[0]?.init.headers as Record<string, string>)['X-Kudurru-CSRF']).toBeUndefined();
    const err = await c.put('/objects/dnszones/a', {}).catch((e: unknown) => e);
    expect((calls[1]?.init.headers as Record<string, string>)['X-Kudurru-CSRF']).toBe('tok');
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).owner).toBe('file:objects.d/zones.yaml');
    expect((err as ApiError).describe()).toBe('zone a is managed elsewhere (managed by file:objects.d/zones.yaml)');
  });

  it('reports the missing permission of a 403 and Retry-After of a 429', () => {
    const forbidden = new ApiError(403, 'forbidden', 'no', { missing: { resource: 'zones', verb: 'write', scope: 'cluster' } });
    expect(forbidden.describe()).toBe('no (needs zones.write at cluster)');
    expect(new ApiError(429, 'rate_limited', 'slow down', undefined, 3).describe()).toBe('slow down (retry in 3s)');
  });

  it('encodes arrays and drops empty values', () => {
    expect(encodeQuery({ a: ['x', 'y'], b: undefined, c: null, d: '', e: 0, f: false })).toBe('?a=x&a=y&e=0&f=false');
  });
});
