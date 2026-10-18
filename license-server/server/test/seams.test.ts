// The signing seam every request and service sees (licensing/types.ts
// SigningState, services/types.ts requireSigner): no signer means 503
// signing_unavailable with the reason, a signer must match the keyring,
// and the System page's kid list is sorted.
import { describe, expect, test } from 'bun:test';
import { HttpError } from '../http/errors';
import { signingReady, signingUnavailable, trustedKids, type Signer } from '../licensing/types';
import { requireSigner } from '../services/types';
import { GOLDEN } from './fixtures';

const signer: Signer = {
  kid: GOLDEN.kid,
  publicKey: GOLDEN.publicKey,
  testKey: true,
  sign: () => Promise.reject(new Error('not in this test')),
};

describe('SigningState', () => {
  test('no signer: 503 signing_unavailable with the reason as hint', () => {
    const signing = signingUnavailable('no signing key is configured', new Map([['b', 'x'], ['a', 'y']]));
    expect(trustedKids(signing)).toEqual(['a', 'b']);
    let err: unknown;
    try {
      requireSigner({ signing });
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).status).toBe(503);
    expect((err as HttpError).body().error).toMatchObject({ key: 'signing_unavailable', hint: 'no signing key is configured' });
  });

  test('a signer must be in the keyring with the same public key', () => {
    const keyring = new Map([[GOLDEN.kid, GOLDEN.publicKey]]);
    expect(requireSigner({ signing: signingReady(signer, keyring) })).toBe(signer);
    expect(signingReady(signer, keyring).unavailableReason).toBeNull();
    expect(() => signingReady(signer, new Map())).toThrow('not in the trusted keyring');
    expect(() => signingReady(signer, new Map([[GOLDEN.kid, 'AAAA']]))).toThrow('not in the trusted keyring');
  });
});
