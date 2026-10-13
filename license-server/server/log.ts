// Logging (design spec §2 "Logs"): JSON lines on stdout,
// `{ts, level, msg, requestId?, ...fields}`. Never log cookies, the CSRF
// header, tokens, client secrets, license keys or the callback query
// string; as a backstop the logger redacts fields with secret-sounding
// names and any string shaped like a signed key (TOKEN_SOURCE). Security
// failures (auth.failed, csrf.denied, authz.denied, signing.refused,
// public.honeypot, public.email_limited) are log lines, not audit rows.
// MemoryLogger records lines for tests.

export type LogLevel = 'debug' | 'info' | 'warn' | 'error';

export const LOG_LEVELS: readonly LogLevel[] = ['debug', 'info', 'warn', 'error'];

/** Extra fields of a line. Error values are written as {name, message, stack}. */
export type LogFields = Readonly<Record<string, unknown>>;

export interface Logger {
  readonly level: LogLevel;
  debug(msg: string, fields?: LogFields): void;
  info(msg: string, fields?: LogFields): void;
  warn(msg: string, fields?: LogFields): void;
  error(msg: string, fields?: LogFields): void;
  /** A logger that adds `fields` to every line (requestId, route). */
  child(fields: LogFields): Logger;
}

/** One written line, parsed. */
export interface LogEntry {
  ts: string;
  level: LogLevel;
  msg: string;
  [field: string]: unknown;
}

export function isLogLevel(v: unknown): v is LogLevel {
  return typeof v === 'string' && (LOG_LEVELS as readonly string[]).includes(v);
}

/** Field names whose values are never written (compared lower-case, ignoring - and _). */
const SECRET_NAMES = new Set(['cookie', 'setcookie', 'authorization', 'csrf', 'xkudurrucsrf', 'token', 'idtoken', 'accesstoken', 'refreshtoken', 'password', 'secret', 'clientsecret', 'seed', 'privatekey', 'signingkey', 'licensekey', 'codeverifier', 'nonce', 'smtpurl', 'databaseurl']);

/**
 * The shape of a compact JWS (a license key, an ID token): three base64url
 * segments, header and payload both JSON objects (so both start "eyJ", the
 * encoding of '{"'), the signature at least 40 characters (Ed25519: 86). A
 * license key is about 300 characters. Tight on purpose: a loose pattern
 * such as eyJ*.+.* matches ordinary names ("MoneyJar.co.uk").
 */
export const TOKEN_SOURCE = String.raw`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{40,}`;

const JWS = new RegExp(TOKEN_SOURCE, 'g');
const HAS_JWS = new RegExp(TOKEN_SOURCE);

/** Whether `s` contains something shaped like a key token (audit.ts refuses such details). */
export function containsToken(s: string): boolean {
  return HAS_JWS.test(s);
}

function normalName(name: string): string {
  return name.toLowerCase().replace(/[-_]/g, '');
}

/** The value as it may be logged: secrets and key-shaped strings redacted, errors flattened. Depth-limited. */
export function redact(value: unknown, depth = 0): unknown {
  if (typeof value === 'string') return value.replace(JWS, '[redacted key]');
  if (value === null || typeof value !== 'object') return value;
  if (depth > 6) return '[deep]';
  if (value instanceof Error) {
    const out: Record<string, unknown> = { name: value.name, message: redact(value.message, depth + 1) };
    if (value.stack) out.stack = redact(value.stack, depth + 1);
    const errno = (value as { errno?: unknown }).errno;
    if (errno !== undefined) out.errno = errno;
    const constraint = (value as { constraint?: unknown }).constraint;
    if (constraint !== undefined) out.constraint = constraint;
    return out;
  }
  if (value instanceof Date) return value.toISOString();
  if (value instanceof Uint8Array) return `[${value.byteLength} bytes]`;
  if (Array.isArray(value)) return value.map((v) => redact(v, depth + 1));
  if (value instanceof Set) return [...value].map((v) => redact(v, depth + 1));
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(value)) out[k] = SECRET_NAMES.has(normalName(k)) ? '[redacted]' : redact(v, depth + 1);
  return out;
}

function line(level: LogLevel, msg: string, base: LogFields, fields: LogFields | undefined, now: () => Date): Record<string, unknown> {
  const out: Record<string, unknown> = { ts: now().toISOString(), level, msg: redact(msg) };
  const extra = redact({ ...base, ...fields }) as Record<string, unknown>;
  for (const [k, v] of Object.entries(extra)) if (k !== 'ts' && k !== 'level' && k !== 'msg' && v !== undefined) out[k] = v;
  return out;
}

class LineLogger implements Logger {
  constructor(
    readonly level: LogLevel,
    private readonly base: LogFields,
    private readonly write: (entry: Record<string, unknown>) => void,
    private readonly now: () => Date,
  ) {}

  private log(level: LogLevel, msg: string, fields?: LogFields): void {
    if (LOG_LEVELS.indexOf(level) < LOG_LEVELS.indexOf(this.level)) return;
    this.write(line(level, msg, this.base, fields, this.now));
  }

  debug(msg: string, fields?: LogFields): void {
    this.log('debug', msg, fields);
  }
  info(msg: string, fields?: LogFields): void {
    this.log('info', msg, fields);
  }
  warn(msg: string, fields?: LogFields): void {
    this.log('warn', msg, fields);
  }
  error(msg: string, fields?: LogFields): void {
    this.log('error', msg, fields);
  }
  child(fields: LogFields): Logger {
    return new LineLogger(this.level, { ...this.base, ...fields }, this.write, this.now);
  }
}

/** The production logger: one JSON line per entry on stdout (or `write`). */
export function jsonLogger(opts: { level?: LogLevel; write?: (line: string) => void; base?: LogFields } = {}): Logger {
  const write = opts.write ?? ((s: string) => void process.stdout.write(s));
  return new LineLogger(
    opts.level ?? 'info',
    opts.base ?? {},
    (entry) => {
      let text: string;
      try {
        text = JSON.stringify(entry);
      } catch {
        text = JSON.stringify({ ts: entry.ts, level: entry.level, msg: entry.msg, logError: 'fields not serialisable' });
      }
      write(text + '\n');
    },
    () => new Date(),
  );
}

/** A logger that keeps every entry (all levels) for assertions. Children share the entry list. */
export class MemoryLogger implements Logger {
  readonly level: LogLevel = 'debug';
  private readonly inner: Logger;

  constructor(
    readonly entries: LogEntry[] = [],
    private readonly base: LogFields = {},
  ) {
    this.inner = new LineLogger('debug', base, (e) => entries.push(e as LogEntry), () => new Date());
  }

  debug(msg: string, fields?: LogFields): void {
    this.inner.debug(msg, fields);
  }
  info(msg: string, fields?: LogFields): void {
    this.inner.info(msg, fields);
  }
  warn(msg: string, fields?: LogFields): void {
    this.inner.warn(msg, fields);
  }
  error(msg: string, fields?: LogFields): void {
    this.inner.error(msg, fields);
  }
  child(fields: LogFields): MemoryLogger {
    return new MemoryLogger(this.entries, { ...this.base, ...fields });
  }

  /** The entries with this message. */
  find(msg: string): LogEntry[] {
    return this.entries.filter((e) => e.msg === msg);
  }

  /** Everything written so far, as the JSON lines production would print (to search for leaks). */
  text(): string {
    return this.entries.map((e) => JSON.stringify(e)).join('\n');
  }

  clear(): void {
    this.entries.length = 0;
  }
}

/** A logger that drops everything. */
export const silentLogger: Logger = {
  level: 'error',
  debug: () => undefined,
  info: () => undefined,
  warn: () => undefined,
  error: () => undefined,
  child: () => silentLogger,
};
