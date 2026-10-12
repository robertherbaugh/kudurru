// Response headers for kudurru.io, written to dist/_headers by build.ts.
// Cloudflare applies _headers to every static asset response; the site has
// no Worker script, so nothing else sets headers.
// https://developers.cloudflare.com/workers/static-assets/headers/

// No inline script or style anywhere on the site; test/build.test.ts checks
// the built pages against this policy.
import { DEFAULT_LICENSE_API } from "./content.ts";

// The one third party the site talks to: the license server of the "Request
// a key" form (connect-src). It also needs CORS to allow https://kudurru.io.
export function csp(licenseApi = DEFAULT_LICENSE_API): string {
  return [
  "default-src 'none'",
  "script-src 'self'",
  "style-src 'self'",
  "img-src 'self'",
  "font-src 'self'",
  `connect-src 'self' ${new URL(licenseApi).origin}`,
  "manifest-src 'self'",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
  ].join("; ");
}

export const CSP = csp();

export function securityHeaders(licenseApi = DEFAULT_LICENSE_API): Record<string, string> {
  return {
  "Content-Security-Policy": csp(licenseApi),
  "Strict-Transport-Security": "max-age=63072000",
  "X-Content-Type-Options": "nosniff",
  "X-Frame-Options": "DENY",
  "Referrer-Policy": "strict-origin-when-cross-origin",
  "Permissions-Policy": "camera=(), microphone=(), geolocation=(), interest-cohort=()",
  "Cross-Origin-Opener-Policy": "same-origin",
  "Cross-Origin-Resource-Policy": "same-origin",
  };
}

export const SECURITY_HEADERS = securityHeaders();

// Path pattern -> headers. Cloudflare joins a header that two matching
// rules both set with a comma, so each header appears in one rule only.
export function rulesFor(licenseApi = DEFAULT_LICENSE_API): [string, Record<string, string>][] {
  return [
    ["/*", securityHeaders(licenseApi)],
    // Every file under /assets/ carries a content hash in its name.
    ["/assets/*", { "Cache-Control": "public, max-age=31536000, immutable" }],
  ];
}

export const RULES = rulesFor();

export function headersFile(rules = RULES): string {
  return rules
    .map(([pattern, headers]) => [pattern, ...Object.entries(headers).map(([k, v]) => `  ${k}: ${v}`)].join("\n"))
    .join("\n\n") + "\n";
}
