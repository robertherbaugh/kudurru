// A fetch stand-in that answers the portal's routes by name (shared/routes.ts),
// for the dev mock (./mock.ts) and the UI tests (stubbed per test with
// vi.stubGlobal through ui/src/test/render.tsx). It lives in ui/src/dev so
// the dev mock depends on nothing under ui/src/test, which the image build
// leaves out.
// Each handler gets the path parameters, the query, the parsed JSON body
// and the headers, and returns a body (200 JSON) or a reply(status, body);
// every call is recorded, and a route without a handler answers 501 and
// lands in `unhandled`, so a test can assert that a page asked for nothing
// it should not. No vitest import: the dev mock runs this in the browser.
// Production builds never load it (main.tsx imports ./mock only in dev).
import { API_PREFIX, ROUTE_LIST, type RouteId } from '@shared/routes';
import type { ErrorKey } from '@shared/api';
import { matchPath } from '../ui/lib/router';

export interface MockRequest {
  route: RouteId;
  method: string;
  path: string;
  params: Record<string, string>;
  query: URLSearchParams;
  body: unknown;
  headers: Headers;
}

const REPLY = Symbol('reply');

export interface MockReply {
  [REPLY]: true;
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
}

/** An answer with a status (204 without a body, an error envelope, a download). */
export function reply(status: number, body?: unknown, headers?: Record<string, string>): MockReply {
  return { [REPLY]: true, status, body, ...(headers ? { headers } : {}) };
}

/** The error envelope of shared/api.ts. */
export function apiError(status: number, key: ErrorKey, message: string, extra: { hint?: unknown; fields?: string[] } = {}): MockReply {
  return reply(status, { error: { key, message, ...extra } });
}

export type MockHandler = (req: MockRequest) => unknown;
export type MockHandlers = Partial<Record<RouteId, MockHandler>>;

export interface FetchMock {
  fetch: typeof fetch;
  calls: MockRequest[];
  unhandled: string[];
  /** The calls of one route. */
  callsTo: (route: RouteId) => MockRequest[];
}

function isReply(v: unknown): v is MockReply {
  return typeof v === 'object' && v !== null && REPLY in v;
}

export function createFetchMock(handlers: MockHandlers): FetchMock {
  const calls: MockRequest[] = [];
  const unhandled: string[] = [];
  const impl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = new URL(typeof input === 'string' ? input : input instanceof URL ? input.href : input.url, 'http://localhost');
    const method = (init?.method ?? 'GET').toUpperCase();
    const at = url.pathname.indexOf(API_PREFIX);
    const path = at >= 0 ? url.pathname.slice(at) : url.pathname;
    const headers = new Headers(init?.headers);
    let body: unknown;
    if (typeof init?.body === 'string' && init.body) {
      try {
        body = JSON.parse(init.body);
      } catch {
        body = init.body;
      }
    }
    for (const r of ROUTE_LIST) {
      if (r.method !== method) continue;
      const params = matchPath(r.path, path);
      if (!params) continue;
      const req: MockRequest = { route: r.id, method, path, params, query: url.searchParams, body, headers };
      calls.push(req);
      const h = handlers[r.id];
      if (!h) {
        unhandled.push(`${method} ${path}`);
        return json(501, { error: { key: 'internal', message: `no mock for ${r.id}` } });
      }
      const out = await h(req);
      if (isReply(out)) {
        if (out.status === 204 || out.body === undefined) return new Response(null, { status: out.status, headers: out.headers });
        if (typeof out.body === 'string') return new Response(out.body, { status: out.status, headers: { 'Content-Type': 'text/plain', ...out.headers } });
        return json(out.status, out.body, out.headers);
      }
      return json(200, out);
    }
    unhandled.push(`${method} ${path}`);
    return json(404, { error: { key: 'not_found', message: 'Not found' } });
  };
  return {
    fetch: impl,
    calls,
    unhandled,
    callsTo: (route) => calls.filter((c) => c.route === route),
  };
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
}
