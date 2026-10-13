// Responses and errors. Handlers return json()/noContent()/redirect()/
// keyFile() and throw HttpError (fail()) for every error answer; the router
// turns anything thrown into the JSON error envelope of shared/api.ts with
// toResponse, which never leaks an internal message: unexpected errors are
// logged with the request id and answered 500 internal with that id as the
// hint. Constraint violations that reach the router become 409/400 without
// detail (handlers should catch the ones they expect and say more).

import { ERROR_STATUS, type ApiErrorBody, type ErrorHint, type ErrorKey } from '../../shared/api';
import { isPgError, PG } from '../db/sql';
import type { Logger } from '../log';

export interface HttpErrorOptions {
  hint?: ErrorHint;
  /** The body fields a 400 invalid failed on. */
  fields?: string[];
  /** Extra response headers (Retry-After on 429, Allow on 405). */
  headers?: Record<string, string>;
  /** The underlying error, logged on 5xx, never sent. */
  cause?: unknown;
}

/** An error answer: status, key, a sentence for people, and the optional hint and fields. */
export class HttpError extends Error {
  override readonly name = 'HttpError';
  readonly hint: ErrorHint | undefined;
  readonly fields: string[] | undefined;
  readonly headers: Record<string, string> | undefined;

  constructor(
    readonly status: number,
    readonly key: ErrorKey,
    message: string,
    opts: HttpErrorOptions = {},
  ) {
    super(message, opts.cause === undefined ? undefined : { cause: opts.cause });
    this.hint = opts.hint;
    this.fields = opts.fields;
    this.headers = opts.headers;
  }

  body(): ApiErrorBody {
    const error: ApiErrorBody['error'] = { key: this.key, message: this.message };
    if (this.hint !== undefined) error.hint = this.hint;
    if (this.fields !== undefined && this.fields.length > 0) error.fields = this.fields;
    return { error };
  }
}

/** Throws the HttpError for `key` with its usual status (ERROR_STATUS). */
export function fail(key: ErrorKey, message: string, opts?: HttpErrorOptions): never {
  throw new HttpError(ERROR_STATUS[key], key, message, opts);
}

/** 404 not_found. */
export function notFound(message = 'Not found.'): never {
  return fail('not_found', message);
}

const JSON_TYPE = 'application/json; charset=utf-8';

/** Status and extra headers of a response. */
export interface ResponseOptions {
  status?: number;
  headers?: Record<string, string>;
}

/** A JSON response (status 200 unless `opts` says otherwise). Type it with the route's ResponseOf<…>. */
export function json<T>(data: T, opts: ResponseOptions = {}): Response {
  return new Response(JSON.stringify(data), {
    status: opts.status ?? 200,
    headers: { ...opts.headers, 'Content-Type': JSON_TYPE },
  });
}

/** 204 No Content. */
export function noContent(headers: Record<string, string> = {}): Response {
  return new Response(null, { status: 204, headers });
}

/** A redirect (302 by default). Pass several Set-Cookie headers as pairs: [['Set-Cookie', a], ['Set-Cookie', b]]. */
export function redirect(location: string, status: 301 | 302 | 303 | 307 | 308 = 302, headers: Record<string, string> | [string, string][] = {}): Response {
  const h = new Headers(headers);
  h.set('Location', location);
  return new Response(null, { status, headers: h });
}

/**
 * A license.key download (spec §6 "Downloads"): the token plus "\n", ASCII,
 * no BOM, as an attachment named license.key. Not cached.
 */
export function keyFile(token: string, filename = 'license.key'): Response {
  return new Response(token + '\n', {
    headers: {
      'Content-Type': 'text/plain; charset=us-ascii',
      'Content-Disposition': `attachment; filename="${filename}"`,
      'Cache-Control': 'no-store',
    },
  });
}

/** The JSON error response for an HttpError. */
export function errorResponse(err: HttpError): Response {
  return json(err.body(), { status: err.status, headers: err.headers ?? {} });
}

/** The answer to an unexpected error: no detail, the request id as the hint, never cached. */
export function internalError(requestId: string): Response {
  const body: ApiErrorBody = { error: { key: 'internal', message: 'Internal error.', hint: requestId } };
  return json(body, { status: 500, headers: { 'Cache-Control': 'no-store' } });
}

/**
 * Turns anything a handler or middleware threw into a response:
 * HttpError as it says; Postgres 23505 (unique) and 23503 (foreign key) →
 * 409 conflict; 23514 (check) and 22P02 (bad text representation) → 400
 * invalid; everything else → 500 internal, logged with its stack.
 */
export function toResponse(err: unknown, requestId: string, log: Logger): Response {
  if (err instanceof HttpError) {
    if (err.status >= 500) log.error('request failed', { status: err.status, key: err.key, message: err.message, err: err.cause });
    return errorResponse(err);
  }
  if (isPgError(err)) {
    const fields = { errno: err.errno, constraint: err.constraint, table: err.table };
    switch (err.errno) {
      case PG.uniqueViolation:
        log.warn('constraint violation', fields);
        return errorResponse(new HttpError(409, 'conflict', 'That conflicts with an existing record.'));
      case PG.foreignKeyViolation:
        log.warn('constraint violation', fields);
        return errorResponse(new HttpError(409, 'conflict', 'That refers to a record that does not exist or is still in use.'));
      case PG.checkViolation:
      case PG.invalidTextRepresentation:
        log.warn('constraint violation', fields);
        return errorResponse(new HttpError(400, 'invalid', 'A value is not valid.'));
    }
  }
  log.error('internal error', { err });
  return internalError(requestId);
}
