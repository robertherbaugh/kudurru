// A Mailer that keeps messages in memory: the tests' mailer (and a dev
// convenience). failNext() makes the next send reject, to test the "mail
// failed after commit" paths.

import { MailNotConfiguredError, type MailMessage, type Mailer } from './types';

export class MemoryMailer implements Mailer {
  readonly sent: MailMessage[] = [];
  private failures: Error[] = [];

  constructor(public configured = true) {}

  send(msg: MailMessage): Promise<void> {
    const failure = this.failures.shift();
    if (failure) return Promise.reject(failure);
    if (!this.configured) return Promise.reject(new MailNotConfiguredError());
    this.sent.push(structuredClone(msg));
    return Promise.resolve();
  }

  /** The next send() rejects with `error` (queued; one per call). */
  failNext(error: Error = new Error('SMTP delivery failed')): void {
    this.failures.push(error);
  }

  /** Messages sent to `to` (compared lower-case). */
  to(to: string): MailMessage[] {
    return this.sent.filter((m) => m.to.toLowerCase() === to.toLowerCase());
  }

  /** The newest message, or undefined. */
  last(): MailMessage | undefined {
    return this.sent.at(-1);
  }

  clear(): void {
    this.sent.length = 0;
    this.failures = [];
  }
}
