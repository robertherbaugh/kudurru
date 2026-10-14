// The mail seam (design spec §6): plain-text messages with optional
// attachments. smtp.ts (nodemailer, SMTP_URL) and memory.ts (tests and dev)
// implement it. Callers send after their transaction commits and log a
// failure instead of failing the request where the spec says so (the key
// stays in the portal and an admin can email it again).

import type { Logger } from '../log';

export interface MailAttachment {
  /** e.g. license.key */
  filename: string;
  /** Text or bytes. A license.key attachment is ASCII with a trailing \n and no BOM. */
  content: string | Uint8Array;
  /** e.g. text/plain; charset=us-ascii */
  contentType: string;
}

export interface MailMessage {
  /** One address. */
  to: string;
  subject: string;
  /** Plain text only; no HTML mail. */
  text: string;
  /** Overrides MAIL_REPLY_TO. */
  replyTo?: string;
  attachments?: MailAttachment[];
}

export interface Mailer {
  /** False when SMTP_URL is unset: send() throws MailNotConfiguredError. */
  readonly configured: boolean;
  /** Submits one message to the mail server; acceptance does not guarantee inbox delivery. */
  send(msg: MailMessage, log?: Logger): Promise<void>;
}

/** send() on a mailer without SMTP_URL. */
export class MailNotConfiguredError extends Error {
  override readonly name = 'MailNotConfiguredError';
  constructor() {
    super('mail is not configured (SMTP_URL is unset)');
  }
}

/** The mailer of a server without SMTP_URL. */
export const unconfiguredMailer: Mailer = {
  configured: false,
  send: () => Promise.reject(new MailNotConfiguredError()),
};
