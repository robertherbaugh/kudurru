// The document an Order Form page shows and downloads: once sent, the text
// the server stored (what the SHA-256 covers), never a fresh rendering of
// the terms; with the acceptances appended once there is any.
import { describe, expect, test } from 'vitest';
import { orderFormDocument } from '@shared/orderForms';
import { executedOrderForm, draftOrderForm, sentOrderForm } from '../dev/fixtures';
import { documentText } from './orderForms';

describe('documentText', () => {
  test('a draft renders its terms', () => {
    expect(documentText(draftOrderForm)).toBe(orderFormDocument({ number: draftOrderForm.number, version: draftOrderForm.version, terms: draftOrderForm.terms }));
  });

  test('a sent form is the stored text, even when its terms would render differently', () => {
    const stored = { ...sentOrderForm, documentText: '# The text as sent\n', terms: { ...sentOrderForm.terms, specialTerms: 'changed since' } };
    expect(documentText(stored)).toBe('# The text as sent\n');
  });

  test('the kept copy is the stored text with both acceptances and the hash', () => {
    const stored = { ...executedOrderForm, documentText: '# The text as sent\n' };
    const text = documentText(stored);
    expect(text.startsWith('# The text as sent\n\n## Acceptance')).toBe(true);
    expect(text).toContain('Customer acceptance: Olga Ops, Head of Operations');
    expect(text).toContain(`Document SHA-256: ${executedOrderForm.documentSha256 ?? ''}`);
  });
});
