// Request bodies and query strings. readJson enforces the JSON-only rule
// (415 unsupported_media_type) and the route's body limit (413
// payload_too_large; both 400 invalid on routes with badBody 'invalid', the
// public form contract), then validates with the small validator kit `v`:
// every failing field is collected, and the answer is one 400 invalid with
// `fields` listing them (dotted paths for nested objects; an array element
// reports its array). Unknown fields are ignored and dropped.
//
// Strings are measured in UTF-16 code units (String.length, as the website
// form and the browser count them). Lone surrogates are always refused
// (Go would turn them into U+FFFD in a key), and so are control characters
// unless a field is multiline. "Blank" uses Go's unicode.IsSpace set, the
// one license.Claims.Validate trims with, so a licensee the portal accepts
// is never refused by a node.

import { DEFAULT_PAGE_LIMIT, MAX_PAGE_LIMIT, type IdKind } from '../../shared/api';
import { isExpiresOn, type ExpiresOn } from '../../shared/dates';
import { DEFAULT_BODY_LIMIT } from '../../shared/routes';
import { isId } from '../db/ids';
import { HttpError } from './errors';
import type { RequestContext } from './types';

// --- Go's unicode.IsSpace and friends ----------------------------------------

/**
 * Go's unicode.IsSpace: '\t', '\n', '\v', '\f', '\r', ' ', U+0085 (NEL),
 * U+00A0 (NBSP) and the other White_Space code points (U+1680,
 * U+2000-U+200A, U+2028, U+2029, U+202F, U+205F, U+3000). Unlike JS's \s it
 * has U+0085 and lacks U+FEFF.
 */
export function isGoSpace(codeUnit: number): boolean {
  switch (codeUnit) {
    case 0x09:
    case 0x0a:
    case 0x0b:
    case 0x0c:
    case 0x0d:
    case 0x20:
    case 0x85:
    case 0xa0:
    case 0x1680:
    case 0x2028:
    case 0x2029:
    case 0x202f:
    case 0x205f:
    case 0x3000:
      return true;
    default:
      return codeUnit >= 0x2000 && codeUnit <= 0x200a;
  }
}

/** Whether `s` is empty or only Go whitespace (strings.TrimSpace(s) == ""). */
export function goBlank(s: string): boolean {
  for (let i = 0; i < s.length; i++) if (!isGoSpace(s.charCodeAt(i))) return false;
  return true;
}

/** Go's strings.TrimSpace. */
export function goTrimSpace(s: string): string {
  let start = 0;
  let end = s.length;
  while (start < end && isGoSpace(s.charCodeAt(start))) start++;
  while (end > start && isGoSpace(s.charCodeAt(end - 1))) end--;
  return s.slice(start, end);
}

/** Whether `s` has a Cc character (U+0000-U+001F, U+007F-U+009F) other than those in `allow`. */
export function hasControlChars(s: string, allow = ''): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if ((c <= 0x1f || (c >= 0x7f && c <= 0x9f)) && !allow.includes(s[i]!)) return true;
  }
  return false;
}

/** Whether `s` has an unpaired UTF-16 surrogate (not representable in UTF-8). */
export function hasLoneSurrogate(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff) {
      const next = s.charCodeAt(i + 1);
      if (next >= 0xdc00 && next <= 0xdfff) {
        i++;
        continue;
      }
      return true;
    }
    if (c >= 0xdc00 && c <= 0xdfff) return true;
  }
  return false;
}

/**
 * Email syntax of the public form (spec §6): at most 254 characters, one
 * @, a non-empty local part, a domain with a dot (not at either end, no
 * empty label), no whitespace or control characters. Expects the trimmed,
 * lower-cased address.
 */
export function isEmailSyntax(s: string): boolean {
  if (s.length === 0 || s.length > 254) return false;
  if (hasControlChars(s) || hasLoneSurrogate(s)) return false;
  const at = s.indexOf('@');
  if (at <= 0 || at !== s.lastIndexOf('@')) return false;
  const local = s.slice(0, at);
  const domain = s.slice(at + 1);
  // One deliverable address and nothing else: a dot-atom local part and
  // hostname labels. Mail libraries re-parse recipient strings, so
  // "bad@x.com,z" or "a<bad@x.com>" would reach bad@x.com while the block
  // list, limits and member rows compare the stored string.
  if (local.length > 64 || !LOCAL_PART.test(local)) return false;
  const labels = domain.split('.');
  if (labels.length < 2 || !labels.every((l) => l.length <= 63 && DOMAIN_LABEL.test(l))) return false;
  return /\p{L}/u.test(labels[labels.length - 1]!);
}

/** Dot-atom (RFC 5322 atext, plus letters and digits beyond ASCII): no quotes, commas, angle brackets, spaces or parentheses. */
const LOCAL_PART = /^[\p{L}\p{N}!#$%&'*+/=?^_`{|}~-]+(?:\.[\p{L}\p{N}!#$%&'*+/=?^_`{|}~-]+)*$/u;
/** A hostname label: letters and digits (any script), hyphens inside. */
const DOMAIN_LABEL = /^[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?$/u;

/** The stored form of an email: Go-trimmed and lower-cased. */
export function normalizeEmail(s: string): string {
  return goTrimSpace(s).toLowerCase();
}

// --- the validator kit --------------------------------------------------------

const INVALID: unique symbol = Symbol('invalid');
type Invalid = typeof INVALID;

/** Parses one value; on failure records the path(s) in `issues` and returns INVALID. */
export interface Validator<T> {
  readonly check: (value: unknown, path: string, issues: string[]) => T | Invalid;
  /** Set by v.optional: the key may be absent. */
  readonly optional?: true;
}

/** The type a validator produces. */
export type Infer<V> = V extends Validator<infer T> ? T : never;

type Shape = Readonly<Record<string, Validator<unknown>>>;

type Simplify<T> = { [K in keyof T]: T[K] } & {};

/** The object type of a shape: optional validators make optional keys. */
export type InferObject<S extends Shape> = Simplify<
  { [K in keyof S as S[K] extends { optional: true } ? never : K]: Infer<S[K]> } & {
    [K in keyof S as S[K] extends { optional: true } ? K : never]?: Infer<S[K]>;
  }
>;

function validator<T>(check: (value: unknown, path: string, issues: string[]) => T | Invalid): Validator<T> {
  return { check };
}

function bad(path: string, issues: string[]): Invalid {
  issues.push(path);
  return INVALID;
}

function join(path: string, key: string): string {
  return path ? `${path}.${key}` : key;
}

export interface StringOptions {
  /** Minimum length in UTF-16 code units (after trim); default 0. */
  min?: number;
  /** Maximum length in UTF-16 code units (after trim). */
  max: number;
  /** Apply Go's strings.TrimSpace first; default false. */
  trim?: boolean;
  /** Refuse strings that are only Go whitespace; default true when min > 0. */
  nonBlank?: boolean;
  /** Allow \t, \n and \r (text areas); other control characters are always refused. Default false. */
  multiline?: boolean;
}

export interface IntOptions {
  min?: number;
  max?: number;
}

export interface ArrayOptions {
  min?: number;
  max?: number;
  /** Refuse duplicate elements (compared with ===). */
  unique?: boolean;
}

/** The validators. Compose them with v.object; run them with validate() or readJson(). */
export const v = {
  string(opts: StringOptions): Validator<string> {
    const min = opts.min ?? 0;
    const nonBlank = opts.nonBlank ?? min > 0;
    const allow = opts.multiline ? '\t\n\r' : '';
    return validator((value, path, issues) => {
      if (typeof value !== 'string') return bad(path, issues);
      const s = opts.trim ? goTrimSpace(value) : value;
      if (hasLoneSurrogate(s) || hasControlChars(s, allow)) return bad(path, issues);
      if (s.length < min || s.length > opts.max) return bad(path, issues);
      if (nonBlank && goBlank(s)) return bad(path, issues);
      return s;
    });
  },

  /** A safe integer (JSON numbers only, never "3"). */
  int(opts: IntOptions = {}): Validator<number> {
    return validator((value, path, issues) => {
      if (typeof value !== 'number' || !Number.isSafeInteger(value)) return bad(path, issues);
      if ((opts.min !== undefined && value < opts.min) || (opts.max !== undefined && value > opts.max)) return bad(path, issues);
      return value;
    });
  },

  bool(): Validator<boolean> {
    return validator((value, path, issues) => (typeof value === 'boolean' ? value : bad(path, issues)));
  },

  /** Exactly this value (noncommercial: v.literal(true) refuses false, "true" and absence). */
  literal<const L extends string | number | boolean>(literal: L): Validator<L> {
    return validator((value, path, issues) => (value === literal ? literal : bad(path, issues)));
  },

  /** One of `values`. */
  enum<const E extends string>(values: readonly E[]): Validator<E> {
    return validator((value, path, issues) => (typeof value === 'string' && (values as readonly string[]).includes(value) ? (value as E) : bad(path, issues)));
  },

  array<T>(item: Validator<T>, opts: ArrayOptions = {}): Validator<T[]> {
    return validator((value, path, issues) => {
      if (!Array.isArray(value)) return bad(path, issues);
      if ((opts.min !== undefined && value.length < opts.min) || (opts.max !== undefined && value.length > opts.max)) return bad(path, issues);
      const out: T[] = [];
      const inner: string[] = [];
      for (const el of value as unknown[]) {
        const r = item.check(el, path, inner);
        if (r !== INVALID) out.push(r);
      }
      if (inner.length > 0) return bad(path, issues);
      if (opts.unique && new Set(out).size !== out.length) return bad(path, issues);
      return out;
    });
  },

  /** The key may be absent (or undefined); null is still refused unless wrapped in v.nullable too. */
  optional<T>(inner: Validator<T>): Validator<T | undefined> & { optional: true } {
    return {
      optional: true,
      check: (value, path, issues) => (value === undefined ? undefined : inner.check(value, path, issues)),
    };
  },

  /** null is allowed. */
  nullable<T>(inner: Validator<T>): Validator<T | null> {
    return validator((value, path, issues) => (value === null ? null : inner.check(value, path, issues)));
  },

  /** An email address: Go-trimmed, lower-cased, then the public form's syntax (isEmailSyntax). */
  email(): Validator<string> {
    return validator((value, path, issues) => {
      if (typeof value !== 'string') return bad(path, issues);
      const s = normalizeEmail(value);
      return isEmailSyntax(s) ? s : bad(path, issues);
    });
  },

  /** A 'YYYY-MM-DD' UTC day. */
  expiresOn(): Validator<ExpiresOn> {
    return validator((value, path, issues) => (isExpiresOn(value) ? value : bad(path, issues)));
  },

  /** An id of this kind (prefix + base64url). */
  id(kind: IdKind): Validator<string> {
    return validator((value, path, issues) => (isId(kind, value) ? value : bad(path, issues)));
  },

  /** An object with these keys; unknown keys are ignored and dropped. */
  object<S extends Shape>(shape: S): Validator<InferObject<S>> {
    return validator((value, path, issues) => {
      if (value === null || typeof value !== 'object' || Array.isArray(value)) return bad(path, issues);
      const input = value as Record<string, unknown>;
      const out: Record<string, unknown> = {};
      const before = issues.length;
      for (const [key, check] of Object.entries(shape)) {
        const has = Object.prototype.hasOwnProperty.call(input, key);
        const r = check.check(has ? input[key] : undefined, join(path, key), issues);
        if (r !== INVALID && r !== undefined) out[key] = r;
      }
      return issues.length > before ? INVALID : (out as InferObject<S>);
    });
  },

  /** A custom check on the output of `inner` (cross-field rules): return false to fail at the inner path. */
  refine<T>(inner: Validator<T>, ok: (value: T) => boolean): Validator<T> {
    return validator((value, path, issues) => {
      const r = inner.check(value, path, issues);
      if (r === INVALID) return r;
      return ok(r) ? r : bad(path, issues);
    });
  },
};

/** The message of a 400 invalid for these fields. */
function invalidMessage(fields: string[]): string {
  return fields.length > 0 ? `Some values are missing or not valid: ${fields.join(', ')}.` : 'The request body must be a JSON object.';
}

/** Runs a validator on an already parsed value; throws 400 invalid with the failing fields. */
export function validate<T>(check: Validator<T>, raw: unknown): T {
  const issues: string[] = [];
  const r = check.check(raw, '', issues);
  if (r === INVALID || issues.length > 0) {
    const fields = [...new Set(issues.filter((p) => p !== ''))];
    throw new HttpError(400, 'invalid', invalidMessage(fields), { fields });
  }
  return r;
}

// --- reading JSON bodies -----------------------------------------------------

/** Whether the request says it carries JSON (media type application/json; parameters ignored). */
export function isJsonContentType(header: string | null): boolean {
  if (!header) return false;
  return header.split(';')[0]!.trim().toLowerCase() === 'application/json';
}

/** Whether the request carries a body: a Content-Length other than 0, or a Transfer-Encoding (chunked). */
export function hasBody(req: Request): boolean {
  if (req.headers.has('transfer-encoding')) return true;
  const length = req.headers.get('content-length');
  return length !== null && Number(length) !== 0;
}

/** The body limit of the request's route. */
export function bodyLimitOf(ctx: Pick<RequestContext, 'route'>): number {
  return ctx.route.bodyLimit ?? DEFAULT_BODY_LIMIT;
}

/** The error for a body the route refuses: as given, or 400 invalid on routes with badBody 'invalid'. */
export function badBody(ctx: Pick<RequestContext, 'route'>, status: 400 | 413 | 415, message: string): HttpError {
  if (ctx.route.badBody === 'invalid') return new HttpError(400, 'invalid', message);
  if (status === 415) return new HttpError(415, 'unsupported_media_type', message);
  if (status === 413) return new HttpError(413, 'payload_too_large', message);
  return new HttpError(400, 'invalid', message);
}

const decoder = new TextDecoder('utf-8', { fatal: true });

/**
 * The request body, read chunk by chunk and abandoned as soon as it passes
 * `limit` (a chunked body has no Content-Length for the router to check):
 * 413 (the route's 400) when too large, 400 when it cannot be read (the
 * client went away mid-body).
 */
async function readBody(ctx: RequestContext, limit: number): Promise<Uint8Array> {
  const stream: ReadableStream<Uint8Array> | null = ctx.req.body;
  if (!stream) return new Uint8Array(0);
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    let chunk: Awaited<ReturnType<typeof reader.read>>;
    try {
      chunk = await reader.read();
    } catch (e) {
      const tooLarge = e instanceof Error && e.message.includes('maxRequestBodySize');
      throw tooLarge ? badBody(ctx, 413, `The body is larger than ${limit} bytes.`) : badBody(ctx, 400, 'The request body could not be read.');
    }
    if (chunk.done) break;
    size += chunk.value.byteLength;
    if (size > limit) {
      void reader.cancel().catch(() => undefined);
      throw badBody(ctx, 413, `The body is larger than ${limit} bytes.`);
    }
    chunks.push(chunk.value);
  }
  return chunks.length === 1 ? chunks[0]! : Buffer.concat(chunks);
}

/**
 * Reads the request body as JSON and validates it: 415 for a content type
 * other than application/json, 413 above the route's body limit (also for
 * chunked bodies, which are cut off there), 400 invalid for malformed
 * UTF-8 or JSON and for failing fields. Call it once per request.
 */
export async function readJson<T>(ctx: RequestContext, parse: Validator<T> | ((raw: unknown) => T)): Promise<T> {
  if (!isJsonContentType(ctx.req.headers.get('content-type'))) throw badBody(ctx, 415, 'Send the body as application/json.');
  const bytes = await readBody(ctx, bodyLimitOf(ctx));
  let raw: unknown;
  try {
    raw = JSON.parse(decoder.decode(bytes));
  } catch {
    throw badBody(ctx, 400, 'The body is not valid JSON.');
  }
  return typeof parse === 'function' ? parse(raw) : validate(parse, raw);
}

// --- query strings -----------------------------------------------------------

function badQuery(name: string): HttpError {
  return new HttpError(400, 'invalid', `The query parameter ${name} is not valid.`, { fields: [name] });
}

/** Query parameter parsers: absent or empty parameters give the fallback; bad ones answer 400 invalid with fields [name]. */
export const q = {
  string(query: URLSearchParams, name: string, opts: { max?: number } = {}): string | undefined {
    const s = query.get(name);
    if (s === null || s === '') return undefined;
    if (s.length > (opts.max ?? 200) || hasControlChars(s) || hasLoneSurrogate(s)) throw badQuery(name);
    return s;
  },

  int(query: URLSearchParams, name: string, opts: { min?: number; max?: number; fallback?: number } = {}): number | undefined {
    const s = query.get(name);
    if (s === null || s === '') return opts.fallback;
    if (!/^-?\d{1,15}$/.test(s)) throw badQuery(name);
    const n = Number(s);
    if ((opts.min !== undefined && n < opts.min) || (opts.max !== undefined && n > opts.max)) throw badQuery(name);
    return n;
  },

  enum<const E extends string>(query: URLSearchParams, name: string, values: readonly E[]): E | undefined {
    const s = query.get(name);
    if (s === null || s === '') return undefined;
    if (!(values as readonly string[]).includes(s)) throw badQuery(name);
    return s as E;
  },

  /** limit (1 to MAX_PAGE_LIMIT, default DEFAULT_PAGE_LIMIT) and offset (≥ 0, default 0). */
  page(query: URLSearchParams): { limit: number; offset: number } {
    return {
      limit: q.int(query, 'limit', { min: 1, max: MAX_PAGE_LIMIT, fallback: DEFAULT_PAGE_LIMIT })!,
      offset: q.int(query, 'offset', { min: 0, max: 1_000_000_000, fallback: 0 })!,
    };
  },
};
