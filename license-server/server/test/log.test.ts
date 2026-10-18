// The logger's backstop redaction (design spec §2 "Logs"): fields with
// secret names and key-shaped strings never reach a log line, while
// ordinary names with dots ("MoneyJar.co.uk") are written as they are.
import { describe, expect, test } from 'bun:test';
import { containsToken, MemoryLogger, redact } from '../log';
import { GOLDEN_VECTORS } from './fixtures';

describe('redact', () => {
  test('key-shaped strings are redacted, names with dots are kept', () => {
    const key = GOLDEN_VECTORS[1]!.key;
    expect(redact(`signed ${key} for MoneyJar.co.uk`)).toBe('signed [redacted key] for MoneyJar.co.uk');
    for (const name of ['MoneyJar.co.uk', 'HockeyJersey.co.uk', 'MoneyJar.co.']) {
      expect(redact(name)).toBe(name);
      expect(containsToken(name)).toBe(false);
    }
    for (const v of GOLDEN_VECTORS) expect(containsToken(v.key)).toBe(true);
  });

  test('secret field names are redacted at any depth', () => {
    const log = new MemoryLogger();
    log.info('x', { licensee: 'HockeyJersey.co.uk', nested: { csrf: 'abc', Set_Cookie: 'kls_session=1', licenseKey: 'whatever' } });
    expect(log.entries[0]).toMatchObject({ licensee: 'HockeyJersey.co.uk', nested: { csrf: '[redacted]', Set_Cookie: '[redacted]', licenseKey: '[redacted]' } });
  });
});
