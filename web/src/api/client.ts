// A small typed fetch wrapper for /api/v1.
//
// - Relative to the detected path prefix (lib/basePath), never a CDN.
// - Cookie sessions: credentials "same-origin" and the X-Kudurru-CSRF header
//   on every mutating request (§8.2); bearer tokens are not used by the UI.
// - Errors become ApiError with the contract's {key, message, hint}.
import type { ApiErrorBody, MissingPermission } from './types';

export type QueryValue = string | number | boolean | undefined | null | readonly string[];
export type Query = Record<string, QueryValue>;

export interface RequestOptions {
  query?: Query | undefined;
  body?: unknown;
  /** Raw body (e.g. BIND text) sent as is with the given content type. */
  text?: string;
  contentType?: string;
  headers?: Record<string, string>;
  signal?: AbortSignal | undefined;
  /** Return the response text instead of parsing JSON. */
  asText?: boolean;
}

/** An error response of the API (or a network failure, status 0). */
export class ApiError extends Error {
  readonly status: number;
  readonly key: string;
  readonly hint: ApiErrorBody['error']['hint'];
  readonly retryAfter: number | undefined;
  /** The failing paths of a 400 invalid (error.fields). */
  readonly fields: string[];

  constructor(status: number, key: string, message: string, hint?: ApiErrorBody['error']['hint'], retryAfter?: number, fields?: string[]) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.key = key;
    this.hint = hint;
    this.retryAfter = retryAfter;
    this.fields = fields ?? [];
  }

  /** The {resource, verb, scope} a 403 says is missing. */
  get missing(): MissingPermission['missing'] | undefined {
    const h = this.hint;
    if (h && typeof h === 'object' && 'missing' in h) {
      return (h as MissingPermission).missing;
    }
    return undefined;
  }

  /** The owner of a 409 managed_by conflict (e.g. file:objects.d/zones.yaml). */
  get owner(): string | undefined {
    return this.key === 'managed_by' && typeof this.hint === 'string' ? this.hint : undefined;
  }

  /** A sentence for a toast or banner. */
  describe(): string {
    const missing = this.missing;
    if (missing) {
      return `${this.message} (needs ${missing.resource}.${missing.verb} at ${missing.scope})`;
    }
    if (this.owner) {
      // Object conflicts name the owner in the message already; settings
      // conflicts carry a how-to-change-it sentence as the hint.
      if (this.message.includes(this.owner)) return this.message;
      return /managed by/i.test(this.message) ? `${this.message}. ${this.owner}` : `${this.message} (managed by ${this.owner})`;
    }
    if (this.retryAfter) {
      return `${this.message} (retry in ${this.retryAfter}s)`;
    }
    if (typeof this.hint === 'string' && this.hint) {
      return `${this.message}: ${this.hint}`;
    }
    return this.message;
  }
}

export interface ClientOptions {
  /** Absolute or root-relative API base, e.g. "/kudurru/api/v1". */
  baseUrl: string;
  /** CSRF token from GET /auth/me. */
  csrf?: () => string | undefined;
  /** Called on any 401 except from the auth routes themselves. */
  onUnauthenticated?: () => void;
  fetch?: typeof fetch;
}

const MUTATING = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

/** Encodes a query object, dropping empty values and repeating arrays. */
export function encodeQuery(query: Query | undefined): string {
  if (!query) return '';
  const sp = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue;
    if (Array.isArray(v)) {
      for (const item of v as readonly string[]) sp.append(k, item);
    } else {
      sp.set(k, String(v));
    }
  }
  const s = sp.toString();
  return s ? `?${s}` : '';
}

/** The API client; one instance per app (see ApiProvider). */
export class ApiClient {
  readonly baseUrl: string;
  private readonly opts: ClientOptions;

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
  }

  /** URL of an API path, for links (export, OIDC start, SSE). */
  url(path: string, query?: Query): string {
    return `${this.baseUrl}${path.startsWith('/') ? path : `/${path}`}${encodeQuery(query)}`;
  }

  async request<T>(method: string, path: string, o: RequestOptions = {}): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json', ...o.headers };
    let body: BodyInit | undefined;
    if (o.text !== undefined) {
      body = o.text;
      headers['Content-Type'] = o.contentType ?? 'text/plain';
    } else if (o.body !== undefined) {
      body = JSON.stringify(o.body);
      headers['Content-Type'] = o.contentType ?? 'application/json';
    }
    if (MUTATING.has(method)) {
      const csrf = this.opts.csrf?.();
      if (csrf) headers['X-Kudurru-CSRF'] = csrf;
    }
    const f = this.opts.fetch ?? globalThis.fetch.bind(globalThis);
    let res: Response;
    try {
      res = await f(this.url(path, o.query), {
        method,
        headers,
        body: body ?? null,
        credentials: 'same-origin',
        signal: o.signal ?? null,
      });
    } catch (err) {
      if (err instanceof DOMException && err.name === 'AbortError') throw err;
      throw new ApiError(0, 'network', 'The kudurru API could not be reached');
    }
    if (!res.ok) {
      throw await toError(res, path, this.opts.onUnauthenticated);
    }
    if (res.status === 204) return undefined as T;
    if (o.asText) return (await res.text()) as T;
    const ct = res.headers.get('Content-Type') ?? '';
    if (!ct.includes('json')) return (await res.text()) as T;
    return (await res.json()) as T;
  }

  get<T>(path: string, query?: Query, signal?: AbortSignal): Promise<T> {
    return this.request<T>('GET', path, { query, signal });
  }
  post<T>(path: string, body?: unknown, query?: Query): Promise<T> {
    return this.request<T>('POST', path, { body, query });
  }
  put<T>(path: string, body: unknown, headers?: Record<string, string>): Promise<T> {
    return this.request<T>('PUT', path, { body, ...(headers ? { headers } : {}) });
  }
  patch<T>(path: string, body: unknown, contentType = 'application/merge-patch+json'): Promise<T> {
    return this.request<T>('PATCH', path, { body, contentType });
  }
  delete<T = void>(path: string, query?: Query): Promise<T> {
    return this.request<T>('DELETE', path, { query });
  }
}

async function toError(res: Response, path: string, onUnauthenticated?: () => void): Promise<ApiError> {
  let parsed: ApiErrorBody | undefined;
  try {
    parsed = (await res.json()) as ApiErrorBody;
  } catch {
    parsed = undefined;
  }
  const retry = Number(res.headers.get('Retry-After') ?? '');
  const e = parsed?.error;
  const err = new ApiError(
    res.status,
    e?.key ?? defaultKey(res.status),
    e?.message ?? (res.statusText || `HTTP ${res.status}`),
    e?.hint,
    Number.isFinite(retry) && retry > 0 ? retry : undefined,
    Array.isArray(e?.fields) ? e.fields : undefined,
  );
  if (res.status === 401 && !path.startsWith('/auth/')) {
    onUnauthenticated?.();
  }
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
    case 412:
      return 'precondition_failed';
    case 429:
      return 'rate_limited';
    case 503:
      return 'unavailable';
    default:
      return 'error';
  }
}
