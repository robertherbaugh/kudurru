import { expect, test } from "bun:test";
import { DEFAULT_LICENSE_API } from "../src/content.ts";
import { CSP, RULES, csp, headersFile, rulesFor } from "../src/headers.ts";

// Cloudflare's limits: 100 rules, 2,000 characters per line.
// https://developers.cloudflare.com/workers/static-assets/headers/
test("_headers stays inside Cloudflare's limits", () => {
  const file = headersFile();
  expect(RULES.length).toBeLessThanOrEqual(100);
  for (const line of file.split("\n")) expect(line.length).toBeLessThan(2000);
});

test("_headers format: a pattern line, then indented Name: value lines", () => {
  expect(headersFile([["/x/*", { A: "1", B: "2" }], ["/*", { C: "3" }]])).toBe("/x/*\n  A: 1\n  B: 2\n\n/*\n  C: 3\n");
});

test("every page gets the security headers, hashed assets a long cache", () => {
  const file = headersFile();
  expect(file).toStartWith("/*\n");
  expect(file).toContain(`  Content-Security-Policy: ${CSP}\n`);
  expect(file).toContain("  Strict-Transport-Security: max-age=63072000\n");
  expect(file).toContain("/assets/*\n  Cache-Control: public, max-age=31536000, immutable\n");
});

test("no header is set by two rules (Cloudflare would join them with a comma)", () => {
  const names = RULES.flatMap(([, headers]) => Object.keys(headers).map((h) => h.toLowerCase()));
  expect(new Set(names).size).toBe(names.length);
});

test("the CSP allows no inline code, and one third party: the license server of the key form", () => {
  expect(CSP).not.toContain("unsafe");
  expect(CSP.match(/https?:\/\/[^\s;]+/g)).toEqual([DEFAULT_LICENSE_API]);
  expect(CSP).toContain(`connect-src 'self' ${DEFAULT_LICENSE_API}`);
  // Only connect-src names it: no script, frame or form may go there.
  for (const d of CSP.split("; ")) if (!d.startsWith("connect-src")) expect(d).not.toContain("http");
  expect(CSP).toContain("form-action 'none'");
});

test("the license server origin follows the build setting, and only its origin", () => {
  expect(csp("https://licenses.example.org/some/path?x=1")).toContain("connect-src 'self' https://licenses.example.org;");
  expect(headersFile(rulesFor("https://licenses.example.org"))).toContain("connect-src 'self' https://licenses.example.org;");
  expect(() => csp("not a url")).toThrow();
});

test("JSON-LD authorization accepts only SHA-256 hashes", () => {
  const hash = "sha256-" + "A".repeat(43) + "=";
  expect(csp(DEFAULT_LICENSE_API, [hash])).toContain(`script-src 'self' '${hash}';`);
  expect(() => csp(DEFAULT_LICENSE_API, ["unsafe-inline"])).toThrow("invalid JSON-LD script hash");
});
