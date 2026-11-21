// Writes through the API with the signed-in admin's session (storage
// state) for specs that need objects of their own: the CSRF token comes
// from GET /auth/me like the SPA's.
import { expect, type APIResponse, type Page } from '@playwright/test';

export interface Api {
  put: (path: string, body: unknown) => Promise<void>;
  del: (path: string) => Promise<void>;
  get: <T>(path: string) => Promise<{ status: number; body: T }>;
  post: <T>(path: string, body: unknown) => Promise<T>;
}

export async function adminApi(page: Page): Promise<Api> {
  const me = await page.request.get('/api/v1/auth/me');
  expect(me.ok(), 'GET /auth/me with the stored session').toBe(true);
  const { csrf } = (await me.json()) as { csrf: string };
  const headers = { 'X-Kudurru-CSRF': csrf };
  return {
    put: async (path, body) => {
      const r = await page.request.put(`/api/v1${path}`, { data: body, headers });
      expect(r.ok(), `PUT ${path}: ${r.status()} ${await r.text()}`).toBe(true);
    },
    del: async (path) => {
      await page.request.delete(`/api/v1${path}`, { headers });
    },
    get: async <T>(path: string) => {
      const r = await page.request.get(`/api/v1${path}`);
      return { status: r.status(), body: (r.ok() ? await r.json() : await r.text()) as T };
    },
    post: async <T>(path: string, body: unknown) => {
      const r = await sendBeforeAdmission(
        () => page.request.post(`/api/v1${path}`, { data: body, headers }),
        `POST ${path}`,
      );
      return (await r.json()) as T;
    },
  };
}

/** A short random suffix for object names of one test. */
export function suffix(): string {
  return Math.random().toString(36).slice(2, 8);
}

export function object(kind: 'BlockList' | 'ClientGroup' | 'Policy', name: string, spec: unknown) {
  return { apiVersion: 'kudurru.io/v1alpha1', kind, metadata: { name, namespace: 'default' }, spec };
}

/**
 * Whether the server has ux-plan C2 (per-source enabled and the inline
 * entries API): the entries of the run's file-managed list answer 200.
 */
export async function hasListSourcesAPI(api: Api): Promise<boolean> {
  const r = await api.get('/blocklists/default/e2e-ads/sources/inline-0/entries?limit=1');
  return r.status === 200;
}

/**
 * Sends one write and repeats it only after the exact refusal that comes
 * before it dispatches the command: either 503 generation_pending while a
 * preceding commit is published, or the exact unavailable response marked
 * Retry-After by the command API. Nothing ran, so this is not a replay. Any
 * other answer, including an unmarked 503 from a command, fails; the bound is
 * ten seconds. The failure message names the status and error key, never a body.
 */
export async function sendBeforeAdmission(send: () => Promise<APIResponse>, what: string): Promise<APIResponse> {
  const deadline = Date.now() + 10_000;
  for (;;) {
    const response = await send();
    if (response.ok()) return response;
    let key: unknown;
    let message: unknown;
    const bytes = await response.body();
    if (bytes.length <= 4096) {
      try {
        const error = (JSON.parse(bytes.toString()) as { error?: { key?: unknown; message?: unknown } }).error;
        key = error?.key;
        message = error?.message;
      } catch { /* No raw body diagnostics. */ }
    }
    const retry = Number(response.headers()['retry-after']);
    const safePreDispatch = key === 'generation_pending'
      || (key === 'unavailable' && message === 'current writable serving generation unavailable');
    if (response.status() !== 503 || !safePreDispatch || !(retry >= 1) || Date.now() + retry * 1000 > deadline) {
      throw new Error(`${what}: HTTP ${response.status()} ${typeof key === 'string' ? key : ''}`);
    }
    console.log(`${what}: 503 ${typeof key === 'string' ? key : ''}, asked again after Retry-After ${retry}s`);
    await new Promise((resolve) => setTimeout(resolve, retry * 1000));
  }
}
