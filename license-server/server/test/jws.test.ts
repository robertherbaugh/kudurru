// The key format against Go (design spec §0.2, §10 "jws"): every vector of
// golden/golden.json (signed by internal/license.Sign) reproduced byte for
// byte, the verifier mirroring license.Verify (whitespace, alg, kid,
// signature, Go's json.Unmarshal and Claims.Validate), Go's escaping, and
// the signer's refusals (features: [], nbf, fractions, lone surrogates).
import { describe, expect, test } from 'bun:test';
import { assertSignable, base64url, cleanToken, encodeClaims, encodeHeader, goJsonString, importSigningKey, signCompact, verify, VerifyError } from '../licensing/jws';
import type { Claims } from '../licensing/types';
import { GOLDEN, GOLDEN_VECTORS } from './fixtures';

const keyring = new Map([[GOLDEN.kid, GOLDEN.publicKey]]);

async function goldenKey(): Promise<CryptoKey> {
  return importSigningKey(new Uint8Array(Buffer.from(GOLDEN.seed, 'base64')));
}

/** A token with this header and payload text, signed by the golden key (for verifier edge cases). */
async function rawToken(header: string, payload: string): Promise<string> {
  const key = await goldenKey();
  const input = `${base64url(header)}.${base64url(payload)}`;
  const sig = new Uint8Array(await crypto.subtle.sign('Ed25519', key, new TextEncoder().encode(input)));
  return `${input}.${base64url(sig)}`;
}

const HEADER = encodeHeader(GOLDEN.kid);

async function verifyError(token: string): Promise<VerifyError> {
  try {
    await verify(token, keyring);
  } catch (e) {
    if (e instanceof VerifyError) return e;
    throw e;
  }
  throw new Error('expected a VerifyError');
}

describe('golden vectors (Go license.Sign)', () => {
  for (const vector of GOLDEN_VECTORS) {
    test(`${vector.name}: byte for byte`, async () => {
      const claims = vector.claims as Claims;
      const [h, p] = vector.key.split('.');
      expect(base64url(encodeHeader(GOLDEN.kid))).toBe(h!);
      expect(base64url(encodeClaims(claims))).toBe(p!);
      if (claims.nbf === undefined) {
        expect(await signCompact(await goldenKey(), GOLDEN.kid, claims)).toBe(vector.key);
      } else {
        // The portal never signs an nbf; the encoding still matches Go's.
        expect(() => assertSignable(GOLDEN.kid, claims)).toThrow('nbf');
      }
      const verified = await verify(vector.key, keyring);
      expect(verified.kid).toBe(GOLDEN.kid);
      expect(verified.claims).toEqual(vector.claims as Claims);
    });
  }
});

describe('Go escaping', () => {
  test('HTML-safe escaping and control characters as encoding/json writes them', () => {
    expect(goJsonString('<a & b>')).toBe('"\\u003ca \\u0026 b\\u003e"');
    expect(goJsonString(String.fromCharCode(0x2028, 0x2029))).toBe('"\\u2028\\u2029"');
    expect(goJsonString('\b\f\n\r\t\x01\x1f')).toBe('"\\b\\f\\n\\r\\t\\u0001\\u001f"');
    expect(goJsonString('"\\/')).toBe('"\\"\\\\/"');
    expect(goJsonString(String.fromCharCode(0x7f))).toBe(`"${String.fromCharCode(0x7f)}"`);
  });

  test('features omitted when absent, nbf omitted when 0', () => {
    const base: Claims = { jti: 'lic_x', iss: 'kudurru', sub: 'A', plan: 'business', nodes: 2, iat: 1, exp: 2 };
    expect(encodeClaims(base)).toBe('{"jti":"lic_x","iss":"kudurru","sub":"A","plan":"business","nodes":2,"iat":1,"exp":2}');
    expect(encodeClaims({ ...base, nbf: 0 })).not.toContain('nbf');
    expect(encodeClaims({ ...base, features: ['oidc'] })).toContain('"nodes":2,"features":["oidc"],"iat":1');
  });
});

describe('signer refusals', () => {
  const base: Claims = { jti: 'lic_AAAAAAAAAAAA', iss: 'kudurru', sub: 'Hank', plan: 'personal', nodes: 3, iat: 100, exp: 200 };
  test.each([
    ['features: []', { features: [] }, 'never empty'],
    ['nbf', { nbf: 50 }, 'nbf'],
    ['fractional iat', { iat: 1.5 }, 'whole number'],
    ['nodes 0', { nodes: 0 }, 'nodes is 0'],
    ['exp not after iat', { exp: 100 }, 'exp is not after iat'],
    ['blank licensee (Go space set)', { sub: ' \u0085\u00a0' }, 'sub (licensee) is empty'],
    ['unknown plan', { plan: 'pro' }, 'plan "pro"'],
    ['lone surrogate', { sub: 'a\ud800' }, 'surrogate'],
    ['unknown feature', { features: ['teleport'] }, 'unknown feature'],
  ] as const)('%s', (_name, patch, message) => {
    expect(() => assertSignable(GOLDEN.kid, { ...base, ...patch } as unknown as Claims)).toThrow(message);
  });
});

describe('verify (license.Verify)', () => {
  const golden = GOLDEN_VECTORS[0]!.key;

  test('whitespace of the Go unicode.IsSpace set is removed anywhere', async () => {
    const wrapped = golden.match(/.{1,40}/g)!.join('\n') + '\u00a0\u0085\t ';
    const v = await verify(` ${wrapped}`, keyring);
    expect(v.token).toBe(golden);
    expect(cleanToken('a\u2028b\u3000c')).toBe('abc');
  });

  test('BOM and zero-width space are not whitespace (ErrMalformed, as on the node)', async () => {
    expect((await verifyError('\ufeff' + golden)).code).toBe('malformed');
    expect((await verifyError('\u200b' + golden)).code).toBe('malformed');
  });

  test('unknown kid, bad signature, padding, wrong alg, JSON serialization', async () => {
    const unknown = await rawToken(encodeHeader('nope'), encodeClaims(GOLDEN_VECTORS[0]!.claims as Claims));
    expect((await verifyError(unknown)).code).toBe('unknown_key');
    const tampered = golden.slice(0, -2) + (golden.endsWith('AA') ? 'BA' : 'AA');
    expect((await verifyError(tampered)).code).toBe('bad_signature');
    expect((await verifyError(golden + '==')).code).toBe('malformed');
    expect((await verifyError(golden.replace(/-/g, '+'))).code).toBe('malformed');
    const ed = await rawToken(`{"alg":"Ed25519","kid":"${GOLDEN.kid}"}`, encodeClaims(GOLDEN_VECTORS[0]!.claims as Claims));
    expect((await verifyError(ed)).code).toBe('malformed');
    expect((await verifyError('{"payload":"x"}')).code).toBe('malformed');
    expect((await verifyError('a.b')).code).toBe('malformed');
    const crit = await rawToken(`{"alg":"EdDSA","crit":["exp"],"kid":"${GOLDEN.kid}"}`, encodeClaims(GOLDEN_VECTORS[0]!.claims as Claims));
    expect((await verifyError(crit)).code).toBe('bad_signature');
  });

  test('header order, extra header fields and missing typ are accepted', async () => {
    const t = await rawToken(`{"typ":"x","kid":"${GOLDEN.kid}","x-server":1,"alg":"EdDSA"}`, encodeClaims(GOLDEN_VECTORS[1]!.claims as Claims));
    expect((await verify(t, keyring)).claims.features).toEqual(['oidc', 'edge', 'audit']);
  });

  test('json.Unmarshal rules: case-insensitive names, unknown claims ignored, null leaves a field unset', async () => {
    const t = await rawToken(HEADER, '{"JTI":"lic_case","Iss":"kudurru","SUB":"Zo\u00eb","plan":"trial","nodes":2,"aud":"x","features":null,"iat":10,"nbf":null,"exp":20}');
    const v = await verify(t, keyring);
    expect(v.claims).toEqual({ jti: 'lic_case', iss: 'kudurru', sub: 'Zo\u00eb', plan: 'trial', nodes: 2, iat: 10, exp: 20 });
    const fold = await rawToken(HEADER, `{"jti":"j","iss":"kudurru","${String.fromCharCode(0x17f)}ub":"long s","plan":"business","nodes":1,"iat":0,"exp":1}`);
    expect((await verify(fold, keyring)).claims.sub).toBe('long s');
  });

  test.each([
    ['1e3 is not an integer for Go', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1e3,"iat":0,"exp":1}'],
    ['3.0 is not an integer for Go', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":3.0,"iat":0,"exp":1}'],
    ['a string number', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":"3","iat":0,"exp":1}'],
    ['int64 overflow', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":9223372036854775808}'],
    ['exp missing', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0}'],
    ['Kudurru is not kudurru', '{"jti":"j","iss":"Kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":1}'],
    ['duplicate key, last wins: nodes 0', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":3,"nodes":0,"iat":0,"exp":1}'],
    ['trailing bytes', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":1} x'],
    ['an array payload', '[1]'],
    ['null payload (zero claims)', 'null'],
    ['a feature that is a number', '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"features":[1],"iat":0,"exp":1}'],
  ])('malformed: %s', async (_name, payload) => {
    const e = await verifyError(await rawToken(HEADER, payload));
    expect(e.code).toBe('malformed');
    expect(e.message).toStartWith('license key is not a valid signed token');
  });

  test('accepted as Go accepts: iat 0, nodes 2^31, unknown features kept, empty features = all', async () => {
    const v = await verify(await rawToken(HEADER, '{"jti":"j","iss":"kudurru","sub":"s","plan":"enterprise","nodes":2147483648,"features":["teleport"],"iat":0,"exp":1}'), keyring);
    expect(v.claims).toEqual({ jti: 'j', iss: 'kudurru', sub: 's', plan: 'enterprise', nodes: 2147483648, features: ['teleport'], iat: 0, exp: 1 });
    const all = await verify(await rawToken(HEADER, '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"features":[],"iat":0,"exp":1}'), keyring);
    expect(all.claims.features).toBeUndefined();
  });

  test('values beyond what the portal can hold are "unsupported", not silently rounded', async () => {
    const e = await verifyError(await rawToken(HEADER, '{"jti":"j","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":9007199254740993}'));
    expect(e.code).toBe('unsupported');
  });
});
