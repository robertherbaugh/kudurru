// The image's HEALTHCHECK (design spec §1): GET http://127.0.0.1:$PORT/readyz
// with a 2 s timeout; exit 0 when it answers 200, 1 otherwise. A separate
// tiny entry (dist/healthcheck.js) so the check never loads the server.

export {};

const port = process.env.PORT ?? '8790';

try {
  const res = await fetch(`http://127.0.0.1:${port}/readyz`, { signal: AbortSignal.timeout(2000) });
  if (!res.ok) console.error(`readyz answered ${res.status}: ${await res.text()}`);
  process.exit(res.ok ? 0 : 1);
} catch (e) {
  console.error(`readyz failed: ${(e as Error).message}`);
  process.exit(1);
}
