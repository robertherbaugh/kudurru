// Helpers of the end-to-end suite (playwright.config.ts): signing in
// through Dex, reading mail from Mailpit (confirmation links, the admins'
// approval mails, key mails), and asking the node's own code
// (server/test/golden/nodestatus.go, internal/license) what a kudurru node
// would make of a key the portal issued. The compose stack of
// license-server/compose.yaml (profile server or e2e) must be running.
import { expect, type Page } from '@playwright/test';
import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { join, resolve } from 'node:path';

/** Every Dex user's password (dev/dex.yaml). */
export const PASSWORD = 'correct horse battery staple';
export const MAILPIT = process.env.E2E_MAILPIT_URL ?? 'http://localhost:8025';
/** The repository root (go run needs the module). */
const REPO = resolve(import.meta.dirname, '..', '..');
/** The keys the compose server trusts (scripts/dev-keygen.ts). */
const TRUSTED = resolve(import.meta.dirname, '..', 'dev', 'trusted-keys');

/** Signs in through Dex as one of the dev users and waits for the portal. */
export async function signIn(page: Page, email: string): Promise<void> {
  await page.goto('/login');
  await page.getByRole('link', { name: /^Sign in with/ }).click();
  await page.waitForURL(/\/dex\//);
  await page.locator('input[name="login"]').fill(email);
  await page.locator('input[name="password"]').fill(PASSWORD);
  await page.locator('button[type="submit"]').click();
  await page.waitForURL((url) => url.port === new URL(page.url()).port && !url.pathname.startsWith('/dex') && !url.pathname.startsWith('/login'));
  await expect(page.getByRole('navigation').first()).toBeVisible();
}

/** Screenshots for the change description: only when E2E_SHOTS names a directory. */
export async function shot(page: Page, name: string): Promise<void> {
  const dir = process.env.E2E_SHOTS;
  if (!dir) return;
  mkdirSync(dir, { recursive: true });
  await page.screenshot({ path: join(dir, `${name}.png`), fullPage: true });
}

interface MailSummary {
  ID: string;
  Subject: string;
  To: { Address: string }[];
  /** How many attachments the message has. */
  Attachments: number;
}

/** A mail as the suite reads it: the plain text and every attachment's content. */
export interface Mail {
  id: string;
  subject: string;
  text: string;
  attachments: { name: string; body: string }[];
}

/**
 * The mails addressed to exactly `to`, newest first. Mailpit's `to:`
 * search matches substrings (admin@ also finds superadmin@), so the list
 * keeps only exact recipients.
 */
export async function listMail(to: string): Promise<MailSummary[]> {
  const res = await fetch(`${MAILPIT}/api/v1/search?query=${encodeURIComponent(`to:"${to}"`)}`);
  const list = (await res.json()) as { messages: MailSummary[] };
  const want = to.toLowerCase();
  return list.messages.filter((m) => m.To.some((r) => r.Address.toLowerCase() === want));
}

async function readMail(id: string, subject: string): Promise<Mail> {
  const msg = (await (await fetch(`${MAILPIT}/api/v1/message/${id}`)).json()) as { Text: string; Attachments: { PartID: string; FileName: string }[] };
  const attachments = await Promise.all(
    msg.Attachments.map(async (a) => ({ name: a.FileName, body: await (await fetch(`${MAILPIT}/api/v1/message/${id}/part/${a.PartID}`)).text() })),
  );
  return { id, subject, text: msg.Text, attachments };
}

/** The newest mail to `to` whose subject matches (and, with `withAttachment`, that has one), waiting up to 15 s. */
export async function waitForMail(to: string, subject: RegExp, opts: { withAttachment?: boolean } = {}): Promise<Mail> {
  const until = Date.now() + 15_000;
  for (;;) {
    const hit = (await listMail(to)).find((m) => subject.test(m.Subject) && (!opts.withAttachment || m.Attachments > 0));
    if (hit) return readMail(hit.ID, hit.Subject);
    if (Date.now() > until) throw new Error(`no mail to ${to} matching ${subject}${opts.withAttachment ? ' with an attachment' : ''}`);
    await new Promise((r) => setTimeout(r, 300));
  }
}

/** The newest key mail to `to`: the one mail kind that carries license.key (the waiting and approval mails do not). */
export async function waitForKeyMail(to: string): Promise<Mail & { key: string }> {
  const mail = await waitForMail(to, /license key/i, { withAttachment: true });
  const key = mail.attachments.find((a) => a.name === 'license.key');
  if (!key) throw new Error(`the mail "${mail.subject}" to ${to} has no license.key`);
  return { ...mail, key: key.body };
}

/** Removes every mail from Mailpit. */
export async function clearMail(): Promise<void> {
  await fetch(`${MAILPIT}/api/v1/messages`, { method: 'DELETE' });
}

/** What a node reports for a key (license.Status plus the CLI's expires date). */
export interface NodeStatus {
  state: string;
  plan?: string;
  licensee?: string;
  id?: string;
  nodes: number;
  features: string[];
  expires?: string;
  error?: string;
}

/**
 * Runs internal/license on the key at `now` (Unix seconds, default now) with
 * the compose server's trusted keys. Skips the assertion when Go is missing,
 * unless REQUIRE_GO=1.
 */
export function nodeStatus(key: string, now = Math.floor(Date.now() / 1000)): NodeStatus | null {
  const res = spawnSync('go', ['run', './license-server/server/test/golden/nodestatus.go', '-trusted', TRUSTED], {
    cwd: REPO,
    input: JSON.stringify([{ key, now }]),
    encoding: 'utf8',
    env: { ...process.env, PATH: `/usr/local/go/bin:${process.env.PATH ?? ''}` },
  });
  if (res.error || res.status !== 0) {
    if (process.env.REQUIRE_GO === '1') throw new Error(`nodestatus failed: ${res.stderr || String(res.error)}`);
    console.warn('go is not available: skipping the node check');
    return null;
  }
  return (JSON.parse(res.stdout) as NodeStatus[])[0]!;
}
