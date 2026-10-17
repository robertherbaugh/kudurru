// The license portal's typed fetch wrapper, adapted from
// dashboard/src/api/client.ts at dd3dcb0 (keep in step by hand).
//
// Changes from the dashboard's client:
// - Routes are named, not spelled: `call('licenseGet', { params: { id } })`
//   builds the path with shared/routes.ts buildPath and types the body and
//   the answer with shared/api.ts RouteTypes, so a URL or a wire type cannot
//   drift from the server's manifest.
// - The base is the API prefix under the detected path prefix (lib/basePath
//   apiBaseFor: "/" → "/api/v1"); buildPath's "/api/v1" is swapped for it.
// - Cookie sessions: credentials "same-origin" and the X-Kudurru-CSRF header
//   (Me.csrf) on every mutating request.
// - Errors become ApiError with the contract's {key, message, hint, fields}
//   and the Retry-After seconds; a 401 outside /auth/* clears the session.
// - No dashboard-only parts (owner, objects, managed_by, raw text bodies).
import type { ApiErrorBody, BodyOf, ErrorHint, ResponseOf } from '@shared/api';
import { isApiErrorBody } from '@shared/api';
import type { Permission } from '@shared/permissions';
import { API_PREFIX, buildPath, ROUTES, type QueryInit, type RouteId, type RouteParams } from '@shared/routes';

/** The message of a network failure (status 0). */
export const UNREACHABLE = 'The license portal could not be reached';

/** An error response of the API (or a network failure, status 0). */
export class ApiError extends Error {
  readonly status: number;
  /** An ErrorKey of shared/api.ts, or 'network' / a status default when the body was not the envelope. */
  readonly key: string;
  readonly hint: ErrorHint | undefined;
  readonly retryAfter: number | undefined;
  /** The failing body fields of a 400 invalid (error.fields, dotted paths). */
  readonly fields: string[];

  constructor(status: number, key: string, message: string, hint?: ErrorHint, retryAfter?: number, fields?: string[]) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.key = key;
    this.hint = hint;
    this.retryAfter = retryAfter;
    this.fields = fields ?? [];
  }

  /** The permission a 403 forbidden says is missing. */
  get missing(): Permission | undefined {
    const h = this.hint;
    if (h && typeof h === 'object' && 'missing' in h) return h.missing;
    return undefined;
  }

  /** Whether a 400 invalid named this body field (or a field under it). */
  hasField(field: string): boolean {
    return this.fields.some((f) => f === field || f.startsWith(`${field}.`));
  }

  /** A sentence for a toast or banner. */
  describe(): string {
    const missing = this.missing;
    if (missing) return `${this.message} (needs ${missing})`;
    if (this.retryAfter) return `${this.message} (try again in ${this.retryAfter} s)`;
    if (typeof this.hint !== 'string' || !this.hint) return this.message;
    // Only 500 internal carries the request id (server/http/errors.ts internalError);
    // any other string hint is a detail, e.g. why no key can be signed (503 signing_unavailable).
    if (this.key === 'internal') return `${this.message} (request ${this.hint})`;
    const ends = /[.!?]$/.test(this.message);
    return ends ? `${this.message.slice(0, -1)} (${this.hint}).` : `${this.message} (${this.hint})`;
  }
}

export interface ClientOptions {
  /** Absolute or root-relative API base, e.g. "/api/v1" or "/portal/api/v1". */
  baseUrl: string;
  /** The CSRF token from GET /auth/me. */
  csrf?: () => string | undefined;
  /** Called on any 401 except from the auth routes themselves. */
  onUnauthenticated?: () => void;
  fetch?: typeof fetch;
}

/** Path parameters: required when the route has some. */
type ParamsArg<Id extends RouteId> = [keyof RouteParams<Id>] extends [never] ? { params?: undefined } : { params: RouteParams<Id> };

/** The JSON body: required when the route takes one. */
type BodyArg<Id extends RouteId> = [BodyOf<Id>] extends [undefined] ? { body?: undefined } : { body: BodyOf<Id> };

/** What `call` takes for a route: its parameters, query, body and an abort signal. */
export type CallArgs<Id extends RouteId> = ParamsArg<Id> & BodyArg<Id> & { query?: QueryInit<Id>; signal?: AbortSignal };

/** Whether `call(id)` may be made without arguments (no parameters, no body). */
type NoArgs<Id extends RouteId> = [keyof RouteParams<Id>] extends [never] ? ([BodyOf<Id>] extends [undefined] ? true : false) : false;

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

/** The API client; one instance per app (see ApiProvider). */
export class ApiClient {
  readonly baseUrl: string;
  private readonly opts: ClientOptions;

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
  }

  /**
   * The URL of an API path from buildPath ("/api/v1/…"), under the base,
   * for links (the sign-in start, key downloads).
   */
  url(path: string): string {
    const rest = path.startsWith(API_PREFIX) ? path.slice(API_PREFIX.length) : path;
    return `${this.baseUrl}${rest.startsWith('/') || rest === '' ? rest : `/${rest}`}`;
  }

  /** The URL of a route (a download or a redirect) for an href. */
  href<Id extends RouteId>(id: Id, params?: RouteParams<Id>, query?: QueryInit<Id>): string {
    const build = buildPath as (id: RouteId, params?: unknown, query?: unknown) => string;
    return this.url(build(id, params, query));
  }

  /** Calls a route by name; resolves with its typed answer (undefined for 204). */
  call<Id extends RouteId>(id: Id, ...rest: NoArgs<Id> extends true ? [args?: CallArgs<Id>] : [args: CallArgs<Id>]): Promise<ResponseOf<Id>> {
    const args = (rest[0] ?? {}) as { params?: unknown; query?: unknown; body?: unknown; signal?: AbortSignal };
    const route = ROUTES[id];
    const build = buildPath as (id: RouteId, params?: unknown, query?: unknown) => string;
    const path = build(id, args.params, args.query);
    return this.request<ResponseOf<Id>>(route.method, path, args.body, args.signal);
  }

  /** A request to an API path (from buildPath). */
  async request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (MUTATING.has(method)) {
      const csrf = this.opts.csrf?.();
      if (csrf) headers['X-Kudurru-CSRF'] = csrf;
    }
    const f = this.opts.fetch ?? globalThis.fetch.bind(globalThis);
    let res: Response;
    try {
      res = await f(this.url(path), {
        method,
        headers,
        body: body === undefined ? null : JSON.stringify(body),
        credentials: 'same-origin',
        signal: signal ?? null,
      });
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err;
      throw new ApiError(0, 'network', UNREACHABLE);
    }
    if (!res.ok) throw await toError(res, path, this.opts.onUnauthenticated);
    if (res.status === 204) return undefined as T;
    const ct = res.headers.get('Content-Type') ?? '';
    if (!ct.includes('json')) return (await res.text()) as T;
    return (await res.json()) as T;
  }
}

async function toError(res: Response, path: string, onUnauthenticated?: () => void): Promise<ApiError> {
  let parsed: unknown;
  try {
    parsed = await res.json();
  } catch {
    parsed = undefined;
  }
  const retry = Number(res.headers.get('Retry-After') ?? '');
  const e: ApiErrorBody['error'] | undefined = isApiErrorBody(parsed) ? parsed.error : undefined;
  const err = new ApiError(
    res.status,
    e?.key ?? defaultKey(res.status),
    e?.message ?? (res.statusText || `HTTP ${res.status}`),
    e?.hint,
    Number.isFinite(retry) && retry > 0 ? retry : undefined,
    Array.isArray(e?.fields) ? e.fields : undefined,
  );
  if (res.status === 401 && !path.startsWith(`${API_PREFIX}/auth/`)) onUnauthenticated?.();
  return err;
}

function defaultKey(status: number): string {
  switch (status) {
    case 400:
      return 'invalid';
    case 401:
      return 'unauthenticated';
    case 403:
      return 'forbidden';
    case 404:
      return 'not_found';
    case 409:
      return 'conflict';
    case 413:
      return 'payload_too_large';
    case 429:
      return 'rate_limited';
    case 503:
      return 'unavailable';
    default:
      return 'internal';
  }
}
