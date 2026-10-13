// In-memory rate limits (design spec §6 "Sign-in", §9): the sign-in start
// and the OIDC callback, 20 a minute each, and the two routes of the
// one-click approval link (preview and decide; anonymous, the token is the
// credential), 30 a minute each, per client address (IPv6 by /64). A token
// bucket per (route, address block): it refills continuously, so a burst of
// 20 (30) is allowed and then one request every three (two) seconds. The
// public form's limits are in Postgres (services), not here, because they
// must hold across restarts and instances.
//
// Beyond the limit the answer carries Retry-After in whole seconds. The
// sign-in routes are browser page loads, and spec §6 wants every callback
// failure to land on /login?error=, never on plain text: the callback, and
// the start when it is a page load, answer 302 /login?error=rate_limited; a
// fetch of the start, and the approval routes, get 429 rate_limited (JSON).
//
// Memory is bounded: when MAX_BUCKETS are kept, buckets that have refilled
// completely are dropped (a full bucket is the same as none) and then the
// least recently used, down to 90 %, so the sweep runs at most once per
// MAX_BUCKETS / 10 new addresses.

import type { RouteId } from '../../shared/routes';
import { isPageLoad, loginErrorRedirect } from '../auth/loginErrors';
import { safeNext } from '../auth/safeNext';
import { HttpError } from './errors';
import { ipBucket } from './ip';
import type { Middleware, RequestContext } from './types';

/** A limit: `max` requests per `windowMs`, refilled continuously. */
export interface RateLimit {
  max: number;
  windowMs: number;
}

/** The limits per address: 20 per minute for the sign-in routes (spec §6), 30 for the approval link's. */
export const DEFAULT_RATE_LIMITS: Partial<Record<RouteId, RateLimit>> = {
  authLogin: { max: 20, windowMs: 60_000 },
  authCallback: { max: 20, windowMs: 60_000 },
  approvalPreview: { max: 30, windowMs: 60_000 },
  approvalDecide: { max: 30, windowMs: 60_000 },
};

/** Buckets kept at most (about 100 bytes each). */
export const MAX_BUCKETS = 50_000;

interface Bucket {
  tokens: number;
  updatedAt: number;
  /** A refusal was logged since the bucket last had a token (one log line per burst). */
  logged: boolean;
}

/** The outcome of one request: allowed, or refused with the seconds until a token is back (firstRefusal: the first since the bucket last had one). */
export type Take = { ok: true } | { ok: false; retryAfter: number; firstRefusal: boolean };

/** A token-bucket limiter keyed by string. */
export class TokenBuckets {
  private readonly buckets = new Map<string, Bucket>();

  constructor(
    private readonly limit: RateLimit,
    private readonly maxBuckets = MAX_BUCKETS,
  ) {}

  /** Takes one token for `key` at `now` (ms). */
  take(key: string, now: number): Take {
    const rate = this.limit.max / this.limit.windowMs;
    const existing = this.buckets.get(key);
    const b: Bucket = existing ?? { tokens: this.limit.max, updatedAt: now, logged: false };
    b.tokens = Math.min(this.limit.max, b.tokens + Math.max(0, now - b.updatedAt) * rate);
    b.updatedAt = now;
    // Most recently used last (a Map keeps insertion order), so eviction takes the least recently used first.
    if (existing) this.buckets.delete(key);
    else this.makeRoom(now);
    this.buckets.set(key, b);
    if (b.tokens >= 1) {
      b.tokens -= 1;
      b.logged = false;
      return { ok: true };
    }
    const firstRefusal = !b.logged;
    b.logged = true;
    // Whole milliseconds first, so float dust (2000.0000000000002 ms) does not add a second.
    return { ok: false, retryAfter: Math.max(1, Math.ceil(Math.round((1 - b.tokens) / rate) / 1000)), firstRefusal };
  }

  /** How many buckets are kept (tests). */
  get size(): number {
    return this.buckets.size;
  }

  /** At the cap: drops the full buckets, then the least recently used down to 90 % of the cap. */
  private makeRoom(now: number): void {
    if (this.buckets.size < this.maxBuckets) return;
    const rate = this.limit.max / this.limit.windowMs;
    for (const [k, old] of this.buckets) {
      if (old.tokens + (now - old.updatedAt) * rate >= this.limit.max) this.buckets.delete(k);
    }
    const target = Math.min(this.maxBuckets - 1, Math.floor(this.maxBuckets * 0.9));
    while (this.buckets.size > target) {
      const oldest = this.buckets.keys().next();
      if (oldest.done) break;
      this.buckets.delete(oldest.value);
    }
  }
}

/**
 * The Pipeline.rateLimit step: routes listed in `limits` (default
 * DEFAULT_RATE_LIMITS) are limited per client address block (ipBucket);
 * every other route passes. The clock is the request's (ctx.now), so
 * setSystemTime moves it in tests.
 */
export function rateLimitStep(limits: Partial<Record<RouteId, RateLimit>> = DEFAULT_RATE_LIMITS): Middleware {
  const limiters = new Map<string, TokenBuckets>();
  for (const [id, limit] of Object.entries(limits) as [RouteId, RateLimit | undefined][]) if (limit) limiters.set(id, new TokenBuckets(limit));
  return (ctx, next) => {
    const limiter = limiters.get(ctx.route.id);
    if (!limiter) return next();
    const key = ipBucket(ctx.ip) ?? 'unknown';
    const r = limiter.take(key, ctx.now.getTime());
    if (r.ok) return next();
    if (r.firstRefusal) ctx.log.warn('rate limited', { block: key, retryAfter: r.retryAfter });
    return Promise.resolve(refusal(ctx, r.retryAfter));
  };
}

/**
 * The answer beyond the limit: 302 /login?error=rate_limited for the
 * callback and for a page load of the start (with its next=), else 429
 * rate_limited; Retry-After on both.
 */
export function refusal(ctx: RequestContext, retryAfter: number): Response {
  const headers = { 'Retry-After': String(retryAfter) };
  if (ctx.route.id === 'authCallback') return loginErrorRedirect('rate_limited', '/', headers);
  if (ctx.route.id === 'authLogin' && isPageLoad(ctx.req)) return loginErrorRedirect('rate_limited', safeNext(ctx.query.get('next')), headers);
  if (ctx.route.id === 'approvalPreview' || ctx.route.id === 'approvalDecide') {
    throw new HttpError(429, 'rate_limited', 'Too many requests from your network. Wait a moment and try again.', { headers });
  }
  throw new HttpError(429, 'rate_limited', 'Too many sign-in attempts from your network. Wait a moment and try again.', { headers });
}
