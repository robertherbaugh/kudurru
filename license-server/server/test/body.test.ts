// The validator kit and the Go-compatible string rules of server/http/body.ts
// (design spec §6 public form): Go's unicode.IsSpace set for "blank", UTF-16
// lengths, control characters and lone surrogates refused, the email
// syntax, every failing field reported at once, unknown fields dropped.
import { describe, expect, test } from 'bun:test';
import { goBlank, goTrimSpace, hasControlChars, hasLoneSurrogate, isEmailSyntax, q, v, validate } from '../http/body';
import { HttpError } from '../http/errors';

function fieldsOf(fn: () => unknown): string[] | undefined {
  try {
    fn();
  } catch (e) {
    expect(e).toBeInstanceOf(HttpError);
    expect((e as HttpError).status).toBe(400);
    return (e as HttpError).fields;
  }
  throw new Error('expected a 400');
}

describe("Go's whitespace", () => {
  // unicode.IsSpace: has NEL (U+0085), lacks BOM (U+FEFF), unlike JS \s.
  test.each(['', ' ', '\t\n\v\f\r', '\u0085', '\u00a0', '\u1680', '\u2000\u200a', '\u2028\u2029', '\u202f\u205f\u3000'])('blank: %j', (s) => {
    expect(goBlank(s)).toBe(true);
  });
  test.each(['a', '\ufeff', '\u200b', ' x '])('not blank: %j', (s) => {
    expect(goBlank(s)).toBe(false);
  });
  test('TrimSpace', () => {
    expect(goTrimSpace('\u0085 Hank\u3000')).toBe('Hank');
    expect(goTrimSpace('\ufeffHank')).toBe('\ufeffHank');
  });
  test('control characters and lone surrogates', () => {
    expect(hasControlChars('a\u0001b')).toBe(true);
    expect(hasControlChars('a\u009fb')).toBe(true);
    expect(hasControlChars('a\nb', '\n')).toBe(false);
    expect(hasControlChars('Zoë 🦉')).toBe(false);
    expect(hasLoneSurrogate('🦉')).toBe(false);
    expect(hasLoneSurrogate('\ud83e')).toBe(true);
    expect(hasLoneSurrogate('x\udd89')).toBe(true);
  });
});

describe('email syntax', () => {
  test.each(['a@b.co', 'first.last+tag@sub.example.org', 'x@example.museum'])('accepts %s', (s) => expect(isEmailSyntax(s)).toBe(true));
  test.each(['', 'a', '@b.co', 'a@b', 'a@@b.co', 'a@b@c.co', 'a b@c.co', 'a@b..co', 'a@.b.co', 'a@b.co.', 'a\u00a0@b.co', `${'a'.repeat(250)}@b.co`])(
    'refuses %j',
    (s) => expect(isEmailSyntax(s)).toBe(false),
  );
  test('v.email trims and lower-cases', () => {
    expect(validate(v.email(), '  Hank@Example.ORG\u3000')).toBe('hank@example.org');
  });
});

describe('validators', () => {
  const form = v.object({
    name: v.string({ min: 1, max: 200 }),
    email: v.email(),
    use: v.string({ min: 1, max: 1000, multiline: true }),
    noncommercial: v.literal(true),
    website: v.optional(v.string({ max: 200 })),
  });

  test('a valid body, unknown fields dropped', () => {
    const out = validate(form, { name: 'Hank', email: 'h@example.org', use: 'my\nhomelab', noncommercial: true, nodes: 5 });
    expect(out).toEqual({ name: 'Hank', email: 'h@example.org', use: 'my\nhomelab', noncommercial: true });
  });

  test('every failing field at once, in shape order', () => {
    expect(fieldsOf(() => validate(form, { name: ' ', email: 'nope', use: '', noncommercial: 'true' }))).toEqual(['name', 'email', 'use', 'noncommercial']);
    expect(fieldsOf(() => validate(form, {}))).toEqual(['name', 'email', 'use', 'noncommercial']);
    expect(fieldsOf(() => validate(form, { name: 'a\u0007', email: 'h@example.org', use: 'x', noncommercial: false }))).toEqual(['name', 'noncommercial']);
  });

  test('a body that is not an object fails without fields', () => {
    expect(fieldsOf(() => validate(form, [1, 2]))).toEqual([]);
    expect(fieldsOf(() => validate(form, null))).toEqual([]);
  });

  // UTF-16 code units, as the browser counts them: an emoji is 2.
  test('string length in UTF-16 code units', () => {
    const two = v.string({ max: 2 });
    expect(validate(two, '🦉')).toBe('🦉');
    expect(fieldsOf(() => validate(two, '🦉a'))).toEqual([]);
  });

  test('nested objects and arrays report dotted paths', () => {
    const issue = v.object({
      features: v.nullable(v.array(v.enum(['oidc', 'edge']), { min: 1, unique: true })),
      newCustomer: v.optional(v.object({ name: v.string({ min: 1, max: 200 }), kind: v.enum(['person', 'organization']) })),
      nodes: v.int({ min: 1, max: 100_000 }),
    });
    expect(validate(issue, { features: null, nodes: 3 })).toEqual({ features: null, nodes: 3 });
    expect(fieldsOf(() => validate(issue, { features: [], newCustomer: { name: 'x', kind: 'bank' }, nodes: 3.5 }))).toEqual(['features', 'newCustomer.kind', 'nodes']);
    expect(fieldsOf(() => validate(issue, { features: ['oidc', 'oidc'], nodes: '3' }))).toEqual(['features', 'nodes']);
  });

  test('expiresOn, ids', () => {
    expect(validate(v.expiresOn(), '2027-10-12')).toBe('2027-10-12');
    expect(fieldsOf(() => validate(v.object({ d: v.expiresOn() }), { d: '2027-02-30' }))).toEqual(['d']);
    expect(validate(v.id('customer'), 'cus_ejB_fnIEG90C')).toBe('cus_ejB_fnIEG90C');
    expect(fieldsOf(() => validate(v.object({ id: v.id('customer') }), { id: 'lic_ejB_fnIEG90C' }))).toEqual(['id']);
  });
});

describe('query parameters', () => {
  test('page defaults and bounds', () => {
    expect(q.page(new URLSearchParams())).toEqual({ limit: 50, offset: 0 });
    expect(q.page(new URLSearchParams('limit=10&offset=20'))).toEqual({ limit: 10, offset: 20 });
    expect(() => q.page(new URLSearchParams('limit=1000'))).toThrow(HttpError);
    expect(() => q.page(new URLSearchParams('offset=-1'))).toThrow(HttpError);
    expect(() => q.int(new URLSearchParams('limit=1e3'), 'limit')).toThrow(HttpError);
  });
  test('enum', () => {
    expect(q.enum(new URLSearchParams('plan=trial'), 'plan', ['personal', 'trial'])).toBe('trial');
    expect(q.enum(new URLSearchParams(''), 'plan', ['personal'])).toBeUndefined();
    expect(() => q.enum(new URLSearchParams('plan=gold'), 'plan', ['personal'])).toThrow(HttpError);
  });
});
