// Provider acknowledgements: Cloudflare can silently drop a suppressed
// recipient with 250 but no server-assigned Message-ID. Nodemailer's own
// Message-ID must not turn that into a successful submission.
import { describe, expect, test } from 'bun:test';
import { HttpError } from '../http/errors';
import { MemoryLogger } from '../log';
import { mailOrUnavailable, smtpMailer, type SmtpReceipt } from '../mail/smtp';
import { GOLDEN_VECTORS, testConfig } from './fixtures';

const recipient = 'private-recipient@example.org';
const clientMessageId = '<generated-by-nodemailer@kudurru.io>';
const providerMessageId = '<assigned-by-cloudflare@kudurru.io>';
const msg = { to: recipient, subject: 'License key', text: GOLDEN_VECTORS[0]!.key };

function acknowledgement(response: string): SmtpReceipt {
  return { response, messageId: clientMessageId, accepted: [recipient], rejected: [] };
}

describe('SMTP provider receipts', () => {
  test('Cloudflare acceptance logs its assigned ID with request context and no credentials, recipient or key', async () => {
    const log = new MemoryLogger();
    const mailer = smtpMailer(testConfig({ mail: { smtpUrl: 'smtps://api_token:smtp-private-password@smtp.mx.cloudflare.net:465' } }), log, () => ({
      sendMail: () => Promise.resolve(acknowledgement(`250 2.0.0 Ok ${providerMessageId}`)),
    }));
    await mailOrUnavailable(mailer, msg, log.child({ requestId: 'req-receipt', licenseId: 'lic_receipt' }));
    expect(log.find('mail accepted by SMTP')).toHaveLength(1);
    expect(log.find('mail accepted by SMTP')[0]).toMatchObject({
      component: 'mail', requestId: 'req-receipt', licenseId: 'lic_receipt', host: 'smtp.mx.cloudflare.net',
      toDomain: 'example.org', messageId: clientMessageId, providerMessageId, responseCode: 250, acceptedCount: 1, rejectedCount: 0,
    });
    for (const secret of ['smtp-private-password', recipient, msg.text]) expect(log.text()).not.toContain(secret);
  });

  test.each(['250 2.0.0 Ok', '250 2.0.0 Ok ', '250 queued'])('Cloudflare response without a provider ID (%s) becomes 503 despite a local ID and accepted recipient', async (response) => {
    const log = new MemoryLogger();
    const mailer = smtpMailer(testConfig({ mail: { smtpUrl: 'smtps://api_token:smtp-private-password@smtp.mx.cloudflare.net:465' } }), log, () => ({
      sendMail: () => Promise.resolve(acknowledgement(response)),
    }));
    try {
      await mailOrUnavailable(mailer, msg, log.child({ requestId: 'req-dropped' }));
      throw new Error('unconfirmed submission succeeded');
    } catch (err) {
      expect(err).toBeInstanceOf(HttpError);
      expect(err).toMatchObject({ status: 503, key: 'unavailable' });
    }
    expect(log.find('mail accepted by SMTP')).toHaveLength(0);
    expect(log.find('mail submission not confirmed: Cloudflare returned no Message-ID')[0]).toMatchObject({ requestId: 'req-dropped', providerMessageId: null, responseCode: 250 });
    for (const secret of ['smtp-private-password', recipient, msg.text]) expect(log.text()).not.toContain(secret);
  });

  test('another SMTP provider can accept mail without assigning an ID in its response', async () => {
    const log = new MemoryLogger();
    const mailer = smtpMailer(testConfig({ mail: { smtpUrl: 'smtps://mail.example.org:465' } }), log, () => ({
      sendMail: () => Promise.resolve(acknowledgement('250 queued')),
    }));
    await mailOrUnavailable(mailer, msg, log);
    expect(log.find('mail accepted by SMTP')[0]).toMatchObject({ messageId: clientMessageId, providerMessageId: null, responseCode: 250 });
  });
});
