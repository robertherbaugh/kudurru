// HTML copy includes untrusted names, use descriptions and admin notes.
// Verify escaping and the links/key information recipients rely on in both
// alternatives; SMTP's real MIME encoding is covered in public.test.ts.
import { describe, expect, test } from 'bun:test';
import { NONCOMMERCIAL_LICENSE_URL } from '../../shared/catalog';
import { brandedMail, PRODUCT_NAME } from '../mail/html';
import {
  confirmationMail,
  keyMail,
  memberInviteMail,
  rejectedMail,
  reviewNeededMail,
  reviewWaitingMail,
  type KeyMailKind,
  type ReviewNeededMail,
} from '../mail/templates';
import { GOLDEN_VECTORS } from './fixtures';

const publicUrl = 'https://licenses.example.org';
const to = 'hank@example.org';
const confirmationUrl = `${publicUrl}/verify?token=confirmation&source=web`;
const inviteUrl = `${publicUrl}/invites/invitation`;
const token = GOLDEN_VECTORS[0]!.key;
const license = { id: 'lic_AAAAAAAAAAAA', licensee: 'Homelab Hank', plan: 'personal', nodes: 3, features: null, expiresOn: '2027-10-12' };
const review: ReviewNeededMail = {
  to,
  publicUrl,
  request: {
    id: 'req_AAAAAAAAAAAA', name: 'Homelab Hank', email: to, use: 'DNS for my home', reason: 'auto_approve_off', source: 'web',
    attestation: { statement: 'Eligible community use', at: '2026-10-12T09:41:00Z', ip: '192.0.2.1' },
  },
  renews: null,
  terms: { nodes: 3, expiresOn: '2027-10-12', holdsUntil: '2026-10-19' },
  approveUrl: `${publicUrl}/approve?token=approval`,
};

describe('branded portal mail', () => {
  test('template drift cannot silently drop body copy, a facts table or the key box', () => {
    const signature = `${PRODUCT_NAME}\n${publicUrl}`;
    const presentation = { title: 'A key', preview: 'Your key is ready.', publicUrl };
    for (const text of [
      'Hello,\n\nImportant final instructions.',
      `Hello,\n\n${signature}\n\nImportant final instructions.`,
      `Hello,\n\nImportant final instructions.\n${publicUrl}`,
    ]) {
      expect(() => brandedMail({ to, subject: 'A key', text }, presentation)).toThrow('mail text must end with the signature paragraph');
    }
    const message = { to, subject: 'A key', text: `Hello,\n\nPlan: personal\nNodes: 3\n\n${token}\n\nKeep these instructions.\n\n${signature}\n` };
    const block = { text: 'Plan: personal\nNodes: 3', rows: [['Plan', 'personal'], ['Nodes', '3']] as const };
    const good = brandedMail(message, { ...presentation, details: [block], key: token });
    expect(good.html).toContain('<th scope="row"');
    expect(good.html).toContain('word-break:break-all');
    expect(good.html).toContain('Keep these instructions.');
    expect(good.text).toBe(message.text);
    expect(() => brandedMail({ ...message, text: message.text.replace('Plan: personal\nNodes: 3', 'Plan: personal\n\nNodes: 3') }, { ...presentation, details: [block] })).toThrow('mail HTML details must match complete text paragraphs');
    expect(() => brandedMail(message, { ...presentation, details: [block, { text: 'Expires: tomorrow', rows: [['Expires', 'tomorrow']] }] })).toThrow('mail HTML details must match complete text paragraphs');
    expect(() => brandedMail({ ...message, text: message.text.replace(token, `Key: ${token}`) }, { ...presentation, key: token })).toThrow('mail HTML key must match a complete text paragraph');
  });

  test('requester and admin copy keeps arbitrary URLs as text while known action and license links stay clickable', () => {
    const suppliedUrl = 'https://requester.example.org/?a=1&b=2';
    const encodedUrl = 'https://requester.example.org/?a=1&amp;b=2';
    const messages = [
      reviewNeededMail({ ...review, request: { ...review.request, name: suppliedUrl, use: `DNS for my home. See ${suppliedUrl}`, attestation: { ...review.request.attestation, statement: suppliedUrl } } }),
      rejectedMail({ to, publicUrl, name: suppliedUrl, note: `See ${suppliedUrl}` }),
      keyMail({ to, publicUrl, kind: 'issued', license: { ...license, licensee: suppliedUrl }, token, inviteUrl }),
    ];
    for (const message of messages) {
      expect(message.text).toContain(suppliedUrl);
      expect(message.html).toContain(encodedUrl);
      expect(message.html).not.toContain(`href="${encodedUrl}"`);
    }
    expect(messages[0]!.html).toContain(`href="${review.approveUrl}"`);
    expect(messages[0]!.html).toContain(`href="${publicUrl}/requests"`);
    expect(messages[2]!.html).toContain(`href="${inviteUrl}"`);
    expect(messages[2]!.html).toContain(`href="${NONCOMMERCIAL_LICENSE_URL}"`);
    const lookalike = `${publicUrl}/requests-untrusted`;
    const message = rejectedMail({ to, publicUrl, name: 'Hank', note: lookalike });
    expect(message.html).toContain(lookalike);
    expect(message.html).not.toContain(`href="${lookalike}"`);
  });

  test('every mail has an embedded logo and complete text alternative, without remote image/font requests', () => {
    const messages = [
      confirmationMail({ to, publicUrl, url: confirmationUrl, approvalRequired: true }),
      ...(['issued', 'renewed', 'resent'] as KeyMailKind[]).map((kind) => keyMail({ to, publicUrl, kind, license, token })),
      reviewWaitingMail({ to, publicUrl, name: license.licensee, approvalRequired: true }),
      reviewNeededMail(review),
      rejectedMail({ to, publicUrl, name: license.licensee, note: 'Please tell us more about your use.' }),
      memberInviteMail({ to, publicUrl, customerName: 'Example', url: inviteUrl, expiresAt: new Date('2026-10-26T09:41:00Z') }),
    ];
    for (const message of messages) {
      expect(message.text).toEndWith(`kudurru license portal\n${publicUrl}\n`);
      expect(message.html).toContain('authority over every zone.');
      expect(message.html).toContain('src="cid:kudurru-logo@kudurru.io"');
      expect(message.html).not.toMatch(/(?:src|href)="(?:https?:\/\/[^" ]+\.(?:png|woff2?)|javascript:)/i);
      const logo = message.attachments!.find((attachment) => attachment.cid === 'kudurru-logo@kudurru.io')!;
      expect(logo.contentDisposition).toBe('inline');
      expect(logo.contentType).toBe('image/png');
      expect(Buffer.from(logo.content).subarray(0, 8)).toEqual(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]));
    }
  });

  test('confirmation preserves approval instructions, expiry and the exact link behind the button', () => {
    for (const approvalRequired of [true, false]) {
      const message = confirmationMail({ to, publicUrl, url: confirmationUrl, approvalRequired });
      expect(message.text).toContain(confirmationUrl);
      expect(message.html).toContain(`href="${publicUrl}/verify?token=confirmation&amp;source=web"`);
      expect(message.html).toContain('Confirm your request');
      expect(message.html).toContain('24 hours and only once');
      expect(message.html).toContain('nothing happens without the confirmation');
      expect(message.html!.includes('once an administrator approves it')).toBe(approvalRequired);
    }
  });

  test('a delivered key retains the exact token, attachment bytes, installation and legal instructions in both alternatives', () => {
    const message = keyMail({ to, publicUrl, kind: 'renewed', approved: true, license, token, inviteUrl });
    expect(message.attachments![0]).toEqual({ filename: 'license.key', content: token + '\n', contentType: 'text/plain; charset=us-ascii' });
    for (const body of [message.text, message.html!]) {
      expect(body).toContain(token);
      expect(body).toContain('KUDURRU_LICENSE_KEY_FILE');
      expect(body).toContain('restart it after changing them');
      expect(body).toContain('kudurru license status');
      expect(body).toContain('Kudurru Community License');
      expect(body).toContain('renew your kudurru license key has been approved');
      expect(body).toContain(inviteUrl);
      expect(body).toContain(license.expiresOn);
    }
    expect(message.html).toContain('Access your licenses');
    expect(message.html).toContain('word-break:break-all');
  });

  test('review details, certification and terms remain visible; blocked requests link only to the signed-in portal', () => {
    const message = reviewNeededMail(review);
    expect(message.html).toContain(`href="${review.approveUrl}"`);
    expect(message.html).toContain('Review this request');
    for (const body of [message.text, message.html!]) {
      expect(body).toContain('Eligible community use');
      expect(body).toContain('2026-10-12 09:41 UTC');
      expect(body).toContain('192.0.2.1');
      expect(body).toContain('2027-10-12 (UTC) if approved by 2026-10-19');
      expect(body).toContain('do not forward this message');
    }
    const blocked = reviewNeededMail({ ...review, request: { ...review.request, reason: 'blocked' }, approveUrl: null });
    expect(blocked.html).not.toContain('/approve?');
    expect(blocked.html).toContain(`href="${publicUrl}/requests"`);
    expect(blocked.html).toContain('The address is on the blocklist.');
  });

  test('names, descriptions, certifications, notes and URL attributes are escaped without changing their plain text', () => {
    const hostile = '<img src=x onerror="alert(1)"> & \'quoted\'';
    const messages = [
      keyMail({ to, publicUrl, kind: 'issued', license: { ...license, licensee: hostile }, token }),
      reviewWaitingMail({ to, publicUrl, name: hostile, approvalRequired: false }),
      rejectedMail({ to, publicUrl, name: hostile, note: `${hostile}\nhttps://example.org/" onmouseover="alert(1)` }),
      reviewNeededMail({ ...review, request: { ...review.request, name: hostile, use: hostile, attestation: { ...review.request.attestation, statement: hostile } } }),
      memberInviteMail({ to, publicUrl, customerName: hostile, url: `${inviteUrl}?label="<test>`, expiresAt: null }),
    ];
    for (const message of messages) {
      expect(message.text).toContain(hostile);
      expect(message.html).not.toContain(hostile);
      expect(message.html).toContain('&lt;img src=x onerror=&quot;alert(1)&quot;&gt; &amp; &#39;quoted&#39;');
      expect(message.html).not.toContain(' onmouseover="');
    }
    expect(messages.at(-1)!.html).toContain('href="https://licenses.example.org/invites/invitation?label=&quot;&lt;test&gt;"');
    const unsafe = memberInviteMail({ to, publicUrl, customerName: 'Example', url: 'javascript:alert(1)', expiresAt: null });
    expect(unsafe.html).not.toContain('href="javascript:');
  });
});
