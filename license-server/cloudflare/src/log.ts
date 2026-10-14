// Log lines of the Worker and the Durable Object, in the server's shape
// ({ts, level, msg, ...}). Workers Logs indexes the fields of a logged
// object. Never log a request's cookies, headers or body, or an env value.

export type Level = 'info' | 'warn' | 'error';

export function log(level: Level, msg: string, fields: Readonly<Record<string, unknown>> = {}): void {
  const line = { ts: new Date().toISOString(), level, msg, source: 'cloudflare-worker', ...fields };
  if (level === 'error') console.error(line);
  else if (level === 'warn') console.warn(line);
  // eslint-disable-next-line no-console -- Workers Logs records console.log as level info
  else console.log(line);
}

/** An error as one short line (name and message, no stack). */
export function describe(err: unknown): string {
  if (err instanceof Error) return `${err.name}: ${err.message}`.slice(0, 500);
  return String(err).slice(0, 500);
}
