// The SMTP mailer (design spec §3 SMTP_URL, MAIL_FROM, MAIL_REPLY_TO):
// nodemailer over smtp:// (STARTTLS, port 587 by default) or smtps://
// (implicit TLS, port 465). Without SMTP_URL the portal runs with
// unconfiguredMailer (nothing is mailed; the public form answers 503).
//
// Security: the URL is parsed here and never handed to nodemailer or a log
// line, so the password stays out of errors; certificates are verified;
// smtp:// requires STARTTLS (requireTLS) except in dev mode and towards a
// loopback relay, so a downgrade cannot expose the credentials or the keys
// in the mails; nodemailer may not read files or URLs named in a message.
// A failed send is logged without the message (it may hold a key) and
// rejected with a short error; mailOrUnavailable turns that into the 503
// unavailable answer where the caller needs the mail to have gone out.

import { createTransport, type SendMailOptions } from 'nodemailer';
import type SMTPTransport from 'nodemailer/lib/smtp-transport';
import type { Config } from '../config';
import { isEmailSyntax } from '../http/body';
import { HttpError } from '../http/errors';
import type { Logger } from '../log';
import { unconfiguredMailer, type MailMessage, type Mailer } from './types';

/** A send that failed: the short reason, never the message or the credentials. */
export class MailDeliveryError extends Error {
  override readonly name = 'MailDeliveryError';
}

/** The parts of SMTP_URL nodemailer needs. Throws a message without the password on a bad URL. */
export interface SmtpTarget {
  host: string;
  port: number;
  /** smtps://: TLS from the first byte. */
  secure: boolean;
  user: string | null;
  pass: string | null;
}

export function parseSmtpUrl(raw: string): SmtpTarget {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new Error('SMTP_URL is not a URL (smtp://user:pass@host:587 or smtps://user:pass@host:465)');
  }
  if (url.protocol !== 'smtp:' && url.protocol !== 'smtps:') throw new Error(`SMTP_URL must start with smtp:// or smtps://, not ${url.protocol}//`);
  if (!url.hostname) throw new Error('SMTP_URL has no host');
  const secure = url.protocol === 'smtps:';
  const port = url.port ? Number(url.port) : secure ? 465 : 587;
  const decode = (s: string): string => {
    try {
      return decodeURIComponent(s);
    } catch {
      throw new Error('SMTP_URL has a badly encoded user name or password');
    }
  };
  return {
    host: url.hostname.replace(/^\[(.*)\]$/, '$1'),
    port,
    secure,
    user: url.username ? decode(url.username) : null,
    pass: url.password ? decode(url.password) : null,
  };
}

function isLoopback(host: string): boolean {
  return host === 'localhost' || host === '::1' || /^127\.\d+\.\d+\.\d+$/.test(host);
}

/** The error fields worth logging (nodemailer sets code, responseCode and command); never the message text of the mail. */
function errorFields(e: unknown): Record<string, unknown> {
  if (!(e instanceof Error)) return { error: String(e) };
  const x = e as Error & { code?: unknown; responseCode?: unknown; command?: unknown };
  return { error: x.message.slice(0, 300), code: x.code ?? null, responseCode: x.responseCode ?? null, command: x.command ?? null };
}

/** The domain of an address, for logs (the full address is personal data). */
function domainOf(address: string): string {
  const at = address.lastIndexOf('@');
  return at >= 0 ? address.slice(at + 1) : '';
}

/** The SMTP result fields used to validate submission and build a receipt log. */
export type SmtpReceipt = Pick<SMTPTransport.SentMessageInfo, 'messageId' | 'response' | 'accepted' | 'rejected'>;

/** The SMTP transport seam: tests can exercise provider receipts without contacting the provider. */
export type SmtpTransportFactory = (options: SMTPTransport.Options, defaults: SendMailOptions) => {
  sendMail(msg: SendMailOptions): Promise<SmtpReceipt>;
};

/** The mailer of SMTP_URL, or unconfiguredMailer without it. Throws on a malformed SMTP_URL (a startup error). */
export function smtpMailer(config: Config, log: Logger, makeTransport: SmtpTransportFactory = createTransport): Mailer {
  if (!config.mail.smtpUrl) return unconfiguredMailer;
  const target = parseSmtpUrl(config.mail.smtpUrl);
  const requireTLS = !target.secure && !config.devMode && !isLoopback(target.host);
  const transport = makeTransport(
    {
      host: target.host,
      port: target.port,
      secure: target.secure,
      requireTLS,
      ...(target.user !== null ? { auth: { user: target.user, pass: target.pass ?? '' } } : {}),
      tls: { servername: target.host, rejectUnauthorized: true, minVersion: 'TLSv1.2' },
      connectionTimeout: 10_000,
      greetingTimeout: 10_000,
      socketTimeout: 30_000,
      disableFileAccess: true,
      disableUrlAccess: true,
      logger: false,
      debug: false,
    },
    { from: config.mail.from, replyTo: config.mail.replyTo },
  );
  const mailLog = log.child({ component: 'mail' });
  mailLog.info('mail configured', { host: target.host, port: target.port, secure: target.secure, requireTLS, from: config.mail.from });

  return {
    configured: true,
    async send(msg: MailMessage, requestLog?: Logger): Promise<void> {
      const sendLog = requestLog ? requestLog.child({ component: 'mail' }) : mailLog;
      // The recipient goes to nodemailer as one address object, never a
      // string it would re-parse into other recipients, and only when it
      // is the single address the portal validated and stored.
      if (!isEmailSyntax(msg.to)) {
        sendLog.error('mail refused: the recipient is not a single plain address', { toDomain: domainOf(msg.to), subject: msg.subject });
        throw new MailDeliveryError('the recipient is not a single plain address');
      }
      try {
        const info = await transport.sendMail({
          to: { name: '', address: msg.to },
          subject: msg.subject,
          text: msg.text,
          ...(msg.replyTo ? { replyTo: msg.replyTo } : {}),
          attachments: (msg.attachments ?? []).map((a) => ({
            filename: a.filename,
            content: typeof a.content === 'string' ? Buffer.from(a.content, 'utf8') : Buffer.from(a.content),
            contentType: a.contentType,
          })),
        });
        // Cloudflare can acknowledge DATA with 250 while dropping every
        // suppressed recipient. Only its server-assigned Message-ID proves
        // submission; nodemailer's info.messageId is generated locally.
        // https://developers.cloudflare.com/email-service/api/send-emails/smtp/#suppressed-recipients
        const cloudflare = target.host.toLowerCase() === 'smtp.mx.cloudflare.net';
        const providerMessageId = cloudflare ? /<([^<>\s]+@[^<>\s]+)>/.exec(info.response)?.[0] ?? null : null;
        const receipt = {
          host: target.host,
          toDomain: domainOf(msg.to),
          subject: msg.subject,
          messageId: info.messageId,
          providerMessageId,
          responseCode: Number(/^\d{3}/.exec(info.response)?.[0]) || null,
          acceptedCount: info.accepted.length,
          rejectedCount: info.rejected.length,
        };
        if (cloudflare && !providerMessageId) {
          sendLog.error('mail submission not confirmed: Cloudflare returned no Message-ID', receipt);
          throw new MailDeliveryError('Cloudflare did not confirm the message; check recipient suppressions');
        }
        sendLog.info('mail accepted by SMTP', receipt);
      } catch (e) {
        if (e instanceof MailDeliveryError) throw e;
        sendLog.error('mail delivery failed', { toDomain: domainOf(msg.to), subject: msg.subject, ...errorFields(e) });
        throw new MailDeliveryError('the mail server did not accept the message');
      }
    },
  };
}

/**
 * Sends now and answers 503 unavailable when it fails (or mail is not
 * configured): for the mails a request cannot succeed without (the
 * public form's confirmation link, "Email key").
 */
export async function mailOrUnavailable(mailer: Mailer, msg: MailMessage, log: Logger, what = 'The mail'): Promise<void> {
  if (!mailer.configured) throw new HttpError(503, 'unavailable', 'Mail is not configured on this portal.');
  try {
    await mailer.send(msg, log);
  } catch (e) {
    if (!(e instanceof MailDeliveryError)) log.error('mail delivery failed', errorFields(e));
    throw new HttpError(503, 'unavailable', `${what} could not be sent right now. Try again later.`);
  }
}
