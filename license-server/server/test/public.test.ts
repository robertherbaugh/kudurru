// The kudurru.io form and its confirmation page (design spec §6 "Public
// form", FROZEN contract; §10 "public flow"): 202 for every valid input,
// 400 invalid with fields (noncommercial missing, false or "true"; content
// type, bad JSON and 8 KiB bodies), the honeypot, the per-IP limit with
// Retry-After, the silent per-email limit, 503 without mail or a signer,
// the confirmation mail sent before the 202 (a failed mail deletes the
// row), the atomic token consume (double confirm, expired link), the
// outcome recorded, and the key mail's license.key (ASCII, trailing
// newline, no BOM). Also the SMTP mailer against a tiny fake SMTP server.
import { afterAll, beforeAll, beforeEach, describe, expect, test } from 'bun:test';
import type { Socket } from 'bun';
import { publicHandlers } from '../api/public';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import { signingUnavailable, type SigningState } from '../licensing/types';
import { silentLogger, MemoryLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { mailOrUnavailable, parseSmtpUrl, smtpMailer } from '../mail/smtp';
import { keyMail } from '../mail/templates';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { GOLDEN_VECTORS, testConfig } from './fixtures';

const handlers = mergeHandlers(publicHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let signing: SigningState;
let app: TestApp;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing });
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@public.example`;
const form = (email: string, extra: Record<string, unknown> = {}) => ({ name: 'Homelab Hank', email, use: 'DNS filtering for my home', noncommercial: true, website: '', ...extra });

async function rows(email: string) {
  return t.db<{ id: string; token_hash: Buffer; verified_at: Date | null; outcome: string | null; license_id: string | null; ip: string | null }[]>`
    select id, token_hash, verified_at, outcome, license_id, host(ip) as ip from public_key_requests where email = ${email} order by created_at`;
}

function tokenFrom(text: string): string {
  const m = /\/verify\?token=([A-Za-z0-9_-]{43})/.exec(text);
  if (!m) throw new Error('no confirmation link in the mail');
  return m[1]!;
}

describeDb('POST /api/v1/public/key-requests', () => {
  beforeEach(async () => {
    await t.db`delete from public_key_requests`;
    app.mailer.clear();
  });

  test('202 {"status":"sent"}: a row with the token hash and a mailed confirmation link', async () => {
    const email = addr('ok');
    const res = await callJson(app, 'publicKeyRequest', { body: form(email.toUpperCase(), { extra: 'ignored', nodes: 50 }) });
    expect(res.status).toBe(202);
    expect(res.body).toEqual({ status: 'sent' });
    const stored = await rows(email);
    expect(stored).toHaveLength(1);
    expect(stored[0]!.ip).toBe('127.0.0.1');
    const mail = app.mailer.to(email);
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toContain('Confirm');
    const token = tokenFrom(mail[0]!.text);
    expect(stored[0]!.token_hash.equals(new Bun.CryptoHasher('sha256').update(token).digest())).toBe(true);
    expect(JSON.stringify(await t.db`select * from public_key_requests`)).not.toContain(token);
    // The unconfirmed address and the client stay out of the append-only log.
    const event = (await t.db<{ actor_label: string; ip: string | null; details: unknown }[]>`
      select actor_label, ip, details from audit_events where action = 'public_request.received' and target_id = ${stored[0]!.id}`)[0]!;
    expect(event).toEqual({ actor_label: 'public form', ip: null, details: {} });
  });

  test.each([
    ['noncommercial missing', { noncommercial: undefined }, ['noncommercial']],
    ['noncommercial false', { noncommercial: false }, ['noncommercial']],
    ['noncommercial "true"', { noncommercial: 'true' }, ['noncommercial']],
    ['a blank name (Go whitespace)', { name: ' \u3000\u0085' }, ['name']],
    ['a name with a control character', { name: 'Hank\u0000' }, ['name']],
    ['a name of 201 characters', { name: 'x'.repeat(201) }, ['name']],
    ['a lone surrogate', { name: 'Hank \ud800' }, ['name']],
    ['an email without a dot in the domain', { email: 'hank@localhost' }, ['email']],
    ['an email with a space', { email: 'ha nk@example.org' }, ['email']],
    ['two @', { email: 'a@b@example.org' }, ['email']],
    ['a blank use', { use: '   ' }, ['use']],
    ['a use of 1001 characters', { use: 'u'.repeat(1001) }, ['use']],
    ['everything wrong', { name: '', email: 'x', use: '', noncommercial: 1 }, ['name', 'email', 'use', 'noncommercial']],
  ])('400 invalid: %s', async (_name, patch, fields) => {
    const res = await callJson(app, 'publicKeyRequest', { body: { ...form(addr('bad')), ...patch } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('invalid');
    expect(res.error?.fields).toEqual(fields);
    expect(app.mailer.sent).toHaveLength(0);
  });

  test('400 invalid with every field for a body that is not an object', async () => {
    const res = await callJson(app, 'publicKeyRequest', { body: ['name'] });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(['name', 'email', 'use', 'noncommercial']);
  });

  test('400 invalid for a wrong content type, bad JSON and bodies over 8 KiB', async () => {
    const text = await call(app, 'publicKeyRequest', { rawBody: JSON.stringify(form(addr('ct'))), headers: { 'Content-Type': 'text/plain' } });
    expect(text.status).toBe(400);
    expect(((await text.json()) as { error: { key: string } }).error.key).toBe('invalid');
    const bad = await call(app, 'publicKeyRequest', { rawBody: '{"name":', headers: { 'Content-Type': 'application/json' } });
    expect(bad.status).toBe(400);
    const big = await call(app, 'publicKeyRequest', { body: form(addr('big'), { padding: 'x'.repeat(9000) }) });
    expect(big.status).toBe(400);
    expect(app.mailer.sent).toHaveLength(0);
  });

  test('the honeypot: 202, nothing stored or sent, a log line', async () => {
    app.log.clear();
    const email = addr('bot');
    const res = await callJson(app, 'publicKeyRequest', { body: form(email, { website: 'http://spam.example' }) });
    expect(res.status).toBe(202);
    expect(await rows(email)).toHaveLength(0);
    expect(app.mailer.sent).toHaveLength(0);
    expect(app.log.find('public.honeypot')).toHaveLength(1);
  });

  test('per IP: 429 rate_limited with Retry-After seconds after PUBLIC_LIMIT_PER_IP_PER_HOUR', async () => {
    for (let i = 0; i < 5; i++) expect((await call(app, 'publicKeyRequest', { body: form(addr('ip')) })).status).toBe(202);
    const res = await callJson(app, 'publicKeyRequest', { body: form(addr('ip')) });
    expect(res.status).toBe(429);
    expect(res.error?.key).toBe('rate_limited');
    const retry = Number(res.res.headers.get('retry-after'));
    expect(retry).toBeGreaterThan(3500);
    expect(retry).toBeLessThanOrEqual(3600);
  });

  test('per IP counts an IPv6 /64 together, and rows older than an hour do not count', async () => {
    const insert = (ip: string, ageMs: number) => t.db`
      insert into public_key_requests (id, name, email, use_description, noncommercial_attested_at, ip, token_hash, created_at, expires_at)
      values (${'pkr_' + crypto.randomUUID().slice(0, 12)}, 'x', ${addr('v6')}, 'x', now(), ${ip}::inet, ${crypto.getRandomValues(new Uint8Array(32))}, ${new Date(Date.now() - ageMs)}, now())`;
    for (let i = 0; i < 5; i++) await insert(`2001:db8:1:2::${i + 1}`, 60_000);
    await insert('127.0.0.1', 2 * 3_600_000);
    const count = await t.db<{ n: string }[]>`select count(*) as n from public_key_requests where ip <<= '2001:db8:1:2::/64'::inet`;
    expect(Number(count[0]!.n)).toBe(5);
    // The test client is 127.0.0.1: its only row is two hours old.
    expect((await call(app, 'publicKeyRequest', { body: form(addr('v4')) })).status).toBe(202);
  });

  test('per email: silent 202 beyond PUBLIC_LIMIT_PER_EMAIL_PER_DAY (nothing stored or sent, a log line)', async () => {
    const email = addr('mail');
    app.log.clear();
    const limited = makeTestApp({ handlers, db: t.db, signing, config: testConfig({ publicForm: { limitPerIpPerHour: 100 } }), log: app.log, mailer: app.mailer });
    try {
      for (let i = 0; i < 3; i++) expect((await call(limited, 'publicKeyRequest', { body: form(email) })).status).toBe(202);
      expect((await call(limited, 'publicKeyRequest', { body: form(email) })).status).toBe(202);
      expect(await rows(email)).toHaveLength(3);
      expect(app.mailer.to(email)).toHaveLength(3);
      expect(app.log.find('public.email_limited')).toHaveLength(1);
    } finally {
      await limited.close();
    }
  });

  test('503 unavailable without mail or without a signer', async () => {
    const noMail = makeTestApp({ handlers, db: t.db, signing, mailer: new MemoryMailer(false) });
    const noKey = makeTestApp({ handlers, db: t.db, signing: signingUnavailable('no signing key is configured') });
    try {
      for (const a of [noMail, noKey]) {
        const res = await callJson(a, 'publicKeyRequest', { body: form(addr('503')) });
        expect(res.status).toBe(503);
        expect(res.error?.key).toBe('unavailable');
      }
    } finally {
      await noMail.close();
      await noKey.close();
    }
  });

  test('a confirmation mail that fails: 503 and the row is deleted (the audit event, which names neither address nor client, stays)', async () => {
    const email = addr('fail');
    const received = async (): Promise<number> => (await t.db<{ one: number }[]>`select 1 as one from audit_events where action = 'public_request.received'`).length;
    const before = await received();
    app.mailer.failNext();
    const res = await callJson(app, 'publicKeyRequest', { body: form(email) });
    expect(res.status).toBe(503);
    expect(res.error?.key).toBe('unavailable');
    expect(await rows(email)).toHaveLength(0);
    expect(await received()).toBe(before + 1);
  });
});

describeDb('POST /api/v1/public/key-requests/verify', () => {
  async function submit(email: string): Promise<string> {
    app.mailer.clear();
    expect((await call(app, 'publicKeyRequest', { body: form(email) })).status).toBe(202);
    return tokenFrom(app.mailer.to(email)[0]!.text);
  }

  beforeEach(async () => {
    await t.db`delete from public_key_requests`;
  });

  test('confirm: outcome issued, the key mailed (ASCII license.key, trailing newline, no BOM), the outcome recorded; never the key in the answer', async () => {
    const email = addr('verify');
    const token = await submit(email);
    app.mailer.clear();
    const res = await call(app, 'publicKeyRequestVerify', { body: { token } });
    const text = await res.text();
    expect(res.status).toBe(200);
    expect(JSON.parse(text)).toEqual({ outcome: 'issued' });
    expect(text).not.toContain('eyJ');
    const row = (await rows(email))[0]!;
    expect(row.verified_at).not.toBeNull();
    expect(row.outcome).toBe('issued');
    const mail = app.mailer.to(email)[0]!;
    const attachment = mail.attachments![0]!;
    const content = attachment.content as string;
    expect(content.charCodeAt(0)).not.toBe(0xfeff);
    expect(content.endsWith('\n')).toBe(true);
    expect(/^[\x21-\x7e]+\n$/.test(content)).toBe(true);
    const stored = (await t.db<{ token: string }[]>`select token from license_keys where license_id = ${row.license_id!}`)[0]!;
    expect(content).toBe(stored.token + '\n');
    expect(mail.text).toContain('/invites/');
    const lic = (await t.db<{ attestation: Record<string, unknown> }[]>`select attestation from licenses where id = ${row.license_id!}`)[0]!;
    expect(lic.attestation).toMatchObject({ source: 'kudurru.io form', ip: '127.0.0.1', email });
  });

  test('a second confirm of the same link is 400 invalid_token; so is a garbage token', async () => {
    const token = await submit(addr('twice'));
    expect((await call(app, 'publicKeyRequestVerify', { body: { token } })).status).toBe(200);
    const again = await callJson(app, 'publicKeyRequestVerify', { body: { token } });
    expect(again.status).toBe(400);
    expect(again.error?.key).toBe('invalid_token');
    expect((await callJson(app, 'publicKeyRequestVerify', { body: { token: 'nope' } })).error?.key).toBe('invalid_token');
  });

  test('two confirms at once: exactly one runs the personal flow', async () => {
    const email = addr('race');
    const token = await submit(email);
    const results = await Promise.all([0, 1, 2].map(() => call(app, 'publicKeyRequestVerify', { body: { token } })));
    expect(results.map((r) => r.status).sort()).toEqual([200, 400, 400]);
    expect(await t.db`select 1 from licenses l join customer_members m on m.customer_id = l.customer_id where m.email = ${email}`).toHaveLength(1);
  });

  test('an expired link is 400 invalid_token', async () => {
    const email = addr('expired');
    const token = await submit(email);
    await t.db`update public_key_requests set expires_at = now() - interval '1 second' where email = ${email}`;
    const res = await callJson(app, 'publicKeyRequestVerify', { body: { token } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('invalid_token');
  });

  test('a confirm that cannot sign keeps the link usable (the consume rolls back)', async () => {
    const email = addr('later');
    const token = await submit(email);
    const noKey = makeTestApp({ handlers, db: t.db, signing: signingUnavailable('no signing key is configured') });
    try {
      expect((await call(noKey, 'publicKeyRequestVerify', { body: { token } })).status).toBe(503);
    } finally {
      await noKey.close();
    }
    expect((await rows(email))[0]!.verified_at).toBeNull();
    expect((await callJson(app, 'publicKeyRequestVerify', { body: { token } })).body).toEqual({ outcome: 'issued' });
  });
});

/** A minimal SMTP server: accepts one session at a time and keeps the DATA of each message. */
function fakeSmtp(): { port: number; messages: string[]; stop: () => void } {
  const messages: string[] = [];
  const state = new Map<Socket<undefined>, { data: boolean; buf: string }>();
  const server = Bun.listen<undefined>({
    hostname: '127.0.0.1',
    port: 0,
    socket: {
      open(s) {
        state.set(s, { data: false, buf: '' });
        s.write('220 fake ESMTP\r\n');
      },
      data(s, chunk) {
        const st = state.get(s)!;
        st.buf += chunk.toString();
        for (;;) {
          if (st.data) {
            const end = st.buf.indexOf('\r\n.\r\n');
            if (end < 0) return;
            messages.push(st.buf.slice(0, end));
            st.buf = st.buf.slice(end + 5);
            st.data = false;
            s.write('250 queued\r\n');
            continue;
          }
          const nl = st.buf.indexOf('\r\n');
          if (nl < 0) return;
          const line = st.buf.slice(0, nl);
          st.buf = st.buf.slice(nl + 2);
          const cmd = line.slice(0, 4).toUpperCase();
          if (cmd === 'EHLO' || cmd === 'HELO') s.write('250-fake\r\n250 8BITMIME\r\n');
          else if (cmd === 'DATA') {
            st.data = true;
            s.write('354 go ahead\r\n');
          } else if (cmd === 'QUIT') {
            s.write('221 bye\r\n');
            s.end();
          } else if (cmd === 'RCPT' && line.includes('reject@')) s.write('550 no such user\r\n');
          else s.write('250 ok\r\n');
        }
      },
      close(s) {
        state.delete(s);
      },
    },
  });
  return { port: server.port, messages, stop: () => server.stop(true) };
}

describe('SMTP mailer', () => {
  test('SMTP_URL parsing keeps the password out of errors', () => {
    expect(parseSmtpUrl('smtp://u%40x:p%3Ass@mail.example:2525')).toEqual({ host: 'mail.example', port: 2525, secure: false, user: 'u@x', pass: 'p:ss' });
    expect(parseSmtpUrl('smtps://mail.example')).toMatchObject({ port: 465, secure: true, user: null });
    expect(parseSmtpUrl('smtp://mail.example')).toMatchObject({ port: 587 });
    expect(() => parseSmtpUrl('http://u:hunter2@mail.example')).toThrow(/smtp:\/\//);
    try {
      parseSmtpUrl('smtp://u:hunter2@');
    } catch (e) {
      expect((e as Error).message).not.toContain('hunter2');
    }
  });

  test('without SMTP_URL: not configured; mailOrUnavailable answers 503', async () => {
    const mailer = smtpMailer(testConfig({ mail: { smtpUrl: null } }), silentLogger);
    expect(mailer.configured).toBe(false);
    let status = 0;
    try {
      await mailOrUnavailable(mailer, { to: 'a@b.example', subject: 's', text: 't' }, silentLogger);
    } catch (e) {
      status = (e as { status: number }).status;
    }
    expect(status).toBe(503);
  });

  test('sends plain text with the license.key attachment; a refused recipient is logged without secrets and rejected', async () => {
    const smtp = fakeSmtp();
    const log = new MemoryLogger();
    try {
      const mailer = smtpMailer(testConfig({ mail: { smtpUrl: `smtp://user:hunter2@127.0.0.1:${smtp.port}` } }), log);
      const token = GOLDEN_VECTORS[0]!.key;
      const msg = keyMail({
        to: 'hank@example.org',
        kind: 'issued',
        license: { id: 'lic_AAAAAAAAAAAA', licensee: 'Homelab Hank', plan: 'personal', nodes: 3, features: null, expiresOn: '2027-10-12' },
        token,
        publicUrl: 'http://localhost:8790',
      });
      await mailer.send(msg);
      expect(smtp.messages).toHaveLength(1);
      const data = smtp.messages[0]!;
      expect(data).toContain('From: kudurru license portal <licenses@kudurru.io>');
      expect(data).toContain('Reply-To: hello@kudurru.io');
      expect(data).toContain('filename=license.key');
      // The attachment, decoded, is the token and a newline.
      const part = data.split('filename=license.key')[1]!;
      const body = part.split('\r\n\r\n')[1]!.split('\r\n----')[0]!.replace(/\r\n/g, '');
      expect(Buffer.from(body, 'base64').toString('latin1')).toBe(token + '\n');

      let err: unknown;
      try {
        await mailer.send({ to: 'reject@example.org', subject: 'Key', text: token });
      } catch (e) {
        err = e;
      }
      expect((err as Error).name).toBe('MailDeliveryError');
      const failed = log.find('mail delivery failed');
      expect(failed).toHaveLength(1);
      expect(failed[0]).toMatchObject({ toDomain: 'example.org', responseCode: 550 });
      expect(log.text()).not.toContain('hunter2');
      expect(log.text()).not.toContain(token);
    } finally {
      smtp.stop();
    }
  });
});
