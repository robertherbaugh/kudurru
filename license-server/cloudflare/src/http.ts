// The request and response handling of the Worker that does not depend on
// the Workers runtime, so `bun test` can run it (test/http.test.ts).
//
// - forContainer: the request the container gets. It is the client's
//   request (method, path, query, body, Cookie, Origin, Sec-Fetch-Site,
//   Content-Type and every other header) with two changes: every header a
//   client could use to claim an address is removed and X-Client-IP is
//   written from CF-Connecting-IP, which Cloudflare's edge sets and clients
//   cannot (the server reads it as CLIENT_IP_HEADER); and the Container
//   class's port switch
//   header is removed, so a client cannot pick another port.
// - fromPlatform: whether a response came from the Container class (its
//   plain-text 500, 503 and 429 when the container cannot start or the
//   connection breaks) rather than from the server, which sets
//   X-Content-Type-Options on every response (design spec §2).
// - unavailable: the 503 the Worker answers instead, in the server's error
//   shape, with the public form routes' CORS headers, never with the cause.

/** The header the server is told to trust (CLIENT_IP_HEADER=X-Client-IP). */
export const CLIENT_IP_HEADER = 'X-Client-IP';

/** Headers that carry a client address; a client-sent copy never reaches the server. */
const ADDRESS_HEADERS = ['x-client-ip', 'x-forwarded-for', 'x-real-ip', 'forwarded', 'true-client-ip'];

/** @cloudflare/containers routes a request with this header to the port it names. */
const TARGET_PORT_HEADER = 'cf-container-target-port';

/**
 * Bodies up to this size are read before forwarding (the server's own
 * limit is 64 KiB, design spec §2): streamed request bodies through the
 * Container class have failed before (cloudflare/containers#116, PATCH).
 * Larger or unsized bodies stream through and the server refuses them.
 */
const BUFFER_LIMIT = 64 * 1024;

/** The kudurru.io form's personal route (design spec §6, FROZEN contract). */
export const PUBLIC_FORM_PATH = '/api/v1/public/key-requests';

/** The kudurru.io form's business route. */
export const PUBLIC_BUSINESS_FORM_PATH = '/api/v1/public/business-requests';

/** The cross-origin routes: the kudurru.io form's two (shared/routes.ts, cors publicForm). */
export const PUBLIC_FORM_PATHS: readonly string[] = [PUBLIC_FORM_PATH, PUBLIC_BUSINESS_FORM_PATH];

/** Seconds a client is asked to wait after a 503. */
export const RETRY_AFTER_SECONDS = '30';

/** What unavailable() needs from the Worker's vars. */
export interface Settings {
  /** PUBLIC_URL: decides HSTS. */
  publicUrl: string;
  /** PUBLIC_FORM_ORIGINS as the server reads it (unset: https://kudurru.io; "-": none). */
  formOrigins: string | undefined;
}

/** CF-Connecting-IP when it looks like an IPv4 or IPv6 address (the server parses it strictly). */
function clientIp(request: Request): string | null {
  const ip = request.headers.get('cf-connecting-ip')?.trim() ?? '';
  return /^[0-9A-Fa-f:.]{2,45}$/.test(ip) ? ip : null;
}

/** A Content-Length within BUFFER_LIMIT. */
function smallBody(request: Request): boolean {
  const length = request.headers.get('content-length');
  return length !== null && /^\d{1,9}$/.test(length) && Number(length) <= BUFFER_LIMIT;
}

/** The request as the container gets it (see the file comment). Redirects are never followed. */
export async function forContainer(request: Request): Promise<Request> {
  const headers = new Headers(request.headers);
  for (const name of ADDRESS_HEADERS) headers.delete(name);
  headers.delete(TARGET_PORT_HEADER);
  const ip = clientIp(request);
  if (ip !== null) headers.set(CLIENT_IP_HEADER, ip);
  // Built from the URL rather than from `request`, so only `headers` count
  // (Bun, which runs the tests, keeps the original headers otherwise).
  const body = request.body !== null && smallBody(request) ? await request.arrayBuffer() : request.body;
  return new Request(request.url, { method: request.method, headers, body, redirect: 'manual' });
}

/** Whether a response was made by the Container class rather than the server (see the file comment). */
export function fromPlatform(res: Response): boolean {
  return (res.status >= 500 || res.status === 429) && !res.headers.has('x-content-type-options');
}

/** A 308 to https when PUBLIC_URL is https and the request came over plain http, else null. */
export function httpsRedirect(request: Request, publicUrl: string): Response | null {
  const url = new URL(request.url);
  if (url.protocol !== 'http:' || !publicUrl.startsWith('https:')) return null;
  url.protocol = 'https:';
  return new Response(null, { status: 308, headers: { Location: url.toString(), 'Cache-Control': 'no-store' } });
}

/** The exact origins of PUBLIC_FORM_ORIGINS, parsed as the server parses them. */
export function formOrigins(raw: string | undefined): string[] {
  const text = (raw ?? '').trim() === '' ? 'https://kudurru.io' : (raw ?? '').trim();
  if (text === '-') return [];
  const out: string[] = [];
  for (const entry of text.split(',')) {
    const value = entry.trim();
    if (value === '') continue;
    try {
      out.push(new URL(value).origin);
    } catch {
      // The server refuses to start with such an entry; nothing to allow here.
    }
  }
  return out;
}

const FORM_MESSAGE = 'Free personal keys cannot be requested right now. Try again later, or write to hello@kudurru.io.';
const API_MESSAGE = 'The license portal is not available right now. It may be starting; try again in a minute.';
const PAGE = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>kudurru license portal</title>
</head>
<body>
<h1>The license portal is not available right now</h1>
<p>It may be starting. Try again in a minute.</p>
<p>If this keeps happening, write to <a href="mailto:hello@kudurru.io">hello@kudurru.io</a>.</p>
</body>
</html>
`;

/**
 * The Worker's own 503: JSON in the server's error shape for /api/ paths
 * (`{"error":{"key":"unavailable","message":...}}`), a small HTML page for
 * everything else. On the public form's routes (personal and business) an
 * allowed Origin gets the same CORS headers the server would send, so the
 * kudurru.io page can read the answer; its preflight gets a 204, so the
 * browser sends the POST, which either reaches a container that has started
 * meanwhile or gets this 503 in a form the page can show.
 */
export function unavailable(request: Request, settings: Settings): Response {
  const url = new URL(request.url);
  const headers = new Headers({
    'Cache-Control': 'no-store',
    'Retry-After': RETRY_AFTER_SECONDS,
    'Content-Security-Policy': "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
    'X-Content-Type-Options': 'nosniff',
    'Referrer-Policy': 'same-origin',
    'X-Frame-Options': 'DENY',
  });
  if (settings.publicUrl.startsWith('https:')) headers.set('Strict-Transport-Security', 'max-age=31536000');

  const isForm = PUBLIC_FORM_PATHS.includes(url.pathname);
  if (isForm) {
    headers.set('Vary', 'Origin');
    headers.set('Access-Control-Expose-Headers', 'Retry-After');
    const origin = request.headers.get('origin');
    if (origin !== null && formOrigins(settings.formOrigins).includes(origin)) {
      headers.set('Access-Control-Allow-Origin', origin);
      if (request.method === 'OPTIONS') {
        headers.set('Access-Control-Allow-Methods', 'POST, OPTIONS');
        headers.set('Access-Control-Allow-Headers', 'Content-Type, Accept');
        return new Response(null, { status: 204, headers });
      }
    }
  }

  const isApi = url.pathname === '/api' || url.pathname.startsWith('/api/');
  let body: string;
  if (isApi) {
    headers.set('Content-Type', 'application/json; charset=utf-8');
    body = JSON.stringify({ error: { key: 'unavailable', message: isForm ? FORM_MESSAGE : API_MESSAGE } });
  } else {
    headers.set('Content-Type', 'text/html; charset=utf-8');
    body = PAGE;
  }
  return new Response(request.method === 'HEAD' ? null : body, { status: 503, headers });
}
