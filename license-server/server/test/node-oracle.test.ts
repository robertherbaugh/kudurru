// The Go node oracle (design spec §10): keys the server signs (the golden
// seed as signer, the trusted dir goldenTrustedKeysDir()) are handed to the
// node's own code (golden/nodestatus.go: license.New, Set, Status) at chosen
// instants. The node must report active, grace and expired exactly where
// the portal's licenseState says so (whole seconds, exp and exp + 30 days),
// print the chosen expiresOn as "expires" (what `kudurru license status`
// shows) and grant the nodes and features the portal issued. Skips without
// Go unless REQUIRE_GO=1, which makes a missing Go a failure.
import { beforeAll, describe, expect, test } from 'bun:test';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { FEATURES, GRACE_SECONDS, licenseState, type LicenseState } from '../../shared/catalog';
import { addDays, expFor, graceEndsOn } from '../../shared/dates';
import { claimsFor, type KeyTerms } from '../licensing/claims';
import { base64url, encodeHeader, importSigningKey, verify, VerifyError } from '../licensing/jws';
import { loadSigning } from '../licensing/keyring';
import type { Signer } from '../licensing/types';
import { silentLogger } from '../log';
import { GOLDEN, GOLDEN_VECTORS, goldenTrustedKeysDir, testConfig } from './fixtures';

const REPO_ROOT = join(import.meta.dir, '..', '..', '..');
const GO = Bun.which('go') ?? (existsSync('/usr/local/go/bin/go') ? '/usr/local/go/bin/go' : null);
const requireGo = process.env.REQUIRE_GO === '1';

if (!GO && requireGo) throw new Error('REQUIRE_GO=1 but no go binary was found (PATH or /usr/local/go/bin/go)');
if (!GO) console.warn('go is not installed: the node oracle tests are skipped (REQUIRE_GO=1 fails instead).');

/** What nodestatus.go prints per case (license.Status plus the CLI's dates). */
interface NodeStatus {
  state: 'community' | 'active' | 'grace' | 'expired' | 'invalid';
  plan?: string;
  licensee?: string;
  id?: string;
  nodes: number;
  features: string[];
  policyLimit: number;
  expires?: string;
  graceEnds?: string;
  error?: string;
  setError?: string;
}

/** Runs the oracle once for every case (one `go run`, so one compile). */
async function nodeStatus(cases: { key: string; now: number }[]): Promise<NodeStatus[]> {
  const proc = Bun.spawn([GO!, 'run', './license-server/server/test/golden/nodestatus.go', '-trusted', goldenTrustedKeysDir()], {
    cwd: REPO_ROOT,
    stdin: new Blob([JSON.stringify(cases)]),
    stdout: 'pipe',
    stderr: 'pipe',
  });
  const [out, err, code] = await Promise.all([new Response(proc.stdout).text(), new Response(proc.stderr).text(), proc.exited]);
  if (code !== 0) throw new Error(`nodestatus.go exited ${code}: ${err}`);
  return JSON.parse(out) as NodeStatus[];
}

/** The node's state for the portal's (expiring is active on a node; withdrawn is the portal's own record). */
function nodeStateOf(s: LicenseState): NodeStatus['state'] {
  return s === 'expiring' ? 'active' : (s as NodeStatus['state']);
}

describe.skipIf(!GO)('Go node oracle', () => {
  let signer: Signer;
  beforeAll(async () => {
    const signing = await loadSigning(testConfig(), silentLogger);
    signer = signing.signer!;
  });

  const signedAt = new Date('2026-10-12T09:30:00Z');
  const keys: { name: string; terms: KeyTerms; features: string[] }[] = [
    { name: 'personal', terms: { licensee: 'Homelab Hank', plan: 'personal', nodes: 3, features: null, expiresOn: addDays('2026-10-12', 365) }, features: FEATURES.map((f) => f.key) },
    { name: 'business subset', terms: { licensee: 'AT&T <Labs>', plan: 'business', nodes: 25, features: ['rbac', 'oidc'], expiresOn: '2028-02-29' }, features: ['oidc', 'rbac'] },
    { name: 'trial', terms: { licensee: 'Zo\u00eb \u6771\u4eac', plan: 'trial', nodes: 1, features: null, expiresOn: addDays('2026-10-12', 60) }, features: FEATURES.map((f) => f.key) },
  ];

  test(
    'active, grace and expired at the exact instants, the expiry printed as chosen, nodes and features as issued',
    async () => {
      const cases: { key: string; now: number; expected: LicenseState; terms: KeyTerms; features: string[] }[] = [];
      for (const k of keys) {
        const key = await signer.sign(claimsFor('lic_ORACLEtest01', k.terms, signedAt));
        const exp = expFor(k.terms.expiresOn);
        for (const now of [Math.floor(signedAt.getTime() / 1000), exp - 1, exp, exp + GRACE_SECONDS - 1, exp + GRACE_SECONDS, exp + GRACE_SECONDS + 86_400]) {
          cases.push({ key, now, expected: licenseState(exp, false, new Date(now * 1000)), terms: k.terms, features: k.features });
        }
      }
      const results = await nodeStatus(cases.map((c) => ({ key: c.key, now: c.now })));
      expect(results).toHaveLength(cases.length);
      results.forEach((r, i) => {
        const c = cases[i]!;
        const where = `${c.terms.plan} at ${new Date(c.now * 1000).toISOString()}`;
        expect({ where, setError: r.setError ?? null }).toEqual({ where, setError: null });
        expect({ where, state: r.state }).toEqual({ where, state: nodeStateOf(c.expected) });
        expect(r.expires).toBe(c.terms.expiresOn);
        expect(r.licensee).toBe(c.terms.licensee);
        expect(r.plan).toBe(c.terms.plan);
        if (r.state === 'expired') {
          expect(r.nodes).toBe(1);
          expect(r.features).toEqual([]);
        } else {
          expect(r.nodes).toBe(c.terms.nodes);
          expect([...r.features].sort()).toEqual([...c.features].sort());
          expect(r.policyLimit).toBe(0);
        }
        if (r.state === 'grace') expect(r.graceEnds).toBe(graceEndsOn(c.terms.expiresOn));
      });
      // The boundaries themselves: the last second of the expiry day is still active, its end starts grace.
      expect(results[1]!.state).toBe('active');
      expect(results[2]!.state).toBe('grace');
      expect(results[4]!.state).toBe('expired');
    },
    180_000,
  );

  test(
    'the TypeScript verifier accepts and refuses exactly what the node does',
    async () => {
      const key = await importSigningKey(new Uint8Array(Buffer.from(GOLDEN.seed, 'base64')));
      const sign = async (header: string, payload: string): Promise<string> => {
        const input = `${base64url(header)}.${base64url(payload)}`;
        return `${input}.${base64url(new Uint8Array(await crypto.subtle.sign('Ed25519', key, new TextEncoder().encode(input))))}`;
      };
      const H = encodeHeader(GOLDEN.kid);
      const P = (body: string) => `{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"business",${body}}`;
      const golden = GOLDEN_VECTORS[0]!.key;
      const cases: [string, string][] = [
        ...GOLDEN_VECTORS.map((v): [string, string] => [v.name, v.key]),
        ['wrapped with Go whitespace', ` ${golden.slice(0, 50)}\n${golden.slice(50, 100)}\u00a0${golden.slice(100)}\u0085\t`],
        ['a BOM in front', `\ufeff${golden}`],
        ['padding', `${golden}==`],
        ['a tampered signature', golden.slice(0, -3) + (golden.endsWith('AAA') ? 'BBB' : 'AAA')],
        ['an unknown kid', await sign(encodeHeader('kudurru-2099-1'), P('"nodes":1,"iat":0,"exp":1'))],
        ['alg Ed25519', await sign(`{"alg":"Ed25519","kid":"${GOLDEN.kid}"}`, P('"nodes":1,"iat":0,"exp":1'))],
        ['header order and extra fields', await sign(`{"x":1,"kid":"${GOLDEN.kid}","alg":"EdDSA"}`, P('"nodes":1,"iat":0,"exp":1'))],
        ['crit', await sign(`{"alg":"EdDSA","crit":["exp"],"kid":"${GOLDEN.kid}"}`, P('"nodes":1,"iat":0,"exp":1'))],
        ['kid not a string', await sign('{"alg":"EdDSA","kid":1}', P('"nodes":1,"iat":0,"exp":1'))],
        ['nodes 1e3', await sign(H, P('"nodes":1e3,"iat":0,"exp":1'))],
        ['nodes 3.0', await sign(H, P('"nodes":3.0,"iat":0,"exp":1'))],
        ['nodes "3"', await sign(H, P('"nodes":"3","iat":0,"exp":1'))],
        ['upper-case claim names', await sign(H, '{"JTI":"j","ISS":"kudurru","Sub":"s","PLAN":"trial","NODES":2,"IAT":0,"EXP":1}')],
        ['long s folds to s', await sign(H, '{"jti":"j","iss":"kudurru","\u017fub":"s","plan":"trial","nodes":2,"iat":0,"exp":1}')],
        ['unknown claims, null nbf and features', await sign(H, P('"aud":"x","nbf":null,"features":null,"nodes":1,"iat":0,"exp":1'))],
        ['features []', await sign(H, P('"features":[],"nodes":1,"iat":0,"exp":1'))],
        ['unknown features only', await sign(H, P('"features":["teleport"],"nodes":1,"iat":0,"exp":1'))],
        ['a lone surrogate escape', await sign(H, '{"jti":"j","iss":"kudurru","sub":"a\\ud800","plan":"trial","nodes":2,"iat":0,"exp":1}')],
        ['exp missing', await sign(H, P('"nodes":1,"iat":0'))],
        ['exp equal to iat', await sign(H, P('"nodes":1,"iat":5,"exp":5'))],
        ['iss Kudurru', await sign(H, '{"jti":"j","iss":"Kudurru","sub":"s","plan":"trial","nodes":1,"iat":0,"exp":1}')],
        ['blank sub (Go space set)', await sign(H, '{"jti":"j","iss":"kudurru","sub":" \\u0085\\u3000","plan":"trial","nodes":1,"iat":0,"exp":1}')],
        ['plan Personal', await sign(H, '{"jti":"j","iss":"kudurru","sub":"s","plan":"Personal","nodes":1,"iat":0,"exp":1}')],
        ['nodes 0', await sign(H, P('"nodes":0,"iat":0,"exp":1'))],
        ['nodes 2^31', await sign(H, P('"nodes":2147483648,"iat":0,"exp":1'))],
        ['int64 overflow', await sign(H, P('"nodes":1,"iat":0,"exp":9223372036854775808'))],
        ['duplicate key, last wins', await sign(H, P('"nodes":3,"nodes":0,"iat":0,"exp":1'))],
        ['trailing bytes', await sign(H, P('"nodes":1,"iat":0,"exp":1') + ' x')],
        ['null payload', await sign(H, 'null')],
        ['array payload', await sign(H, '[1]')],
        ['empty payload', await sign(H, '')],
      ];
      const go = await nodeStatus(cases.map(([, k]) => ({ key: k, now: 1 })));
      const goCode = (r: NodeStatus): string => {
        if (!r.setError) return 'ok';
        if (r.setError.startsWith('license key is not a valid signed token')) return 'malformed';
        if (r.setError.startsWith('license key is signed with a key this build does not trust')) return 'unknown_key';
        if (r.setError.startsWith('license key signature does not verify')) return 'bad_signature';
        return r.setError;
      };
      const keyring = new Map([[GOLDEN.kid, GOLDEN.publicKey]]);
      for (let i = 0; i < cases.length; i++) {
        const [name, token] = cases[i]!;
        let ts = 'ok';
        try {
          await verify(token, keyring);
        } catch (e) {
          ts = e instanceof VerifyError ? e.code : String(e);
        }
        expect({ name, code: ts }).toEqual({ name, code: goCode(go[i]!) });
      }
    },
    180_000,
  );

  test(
    'a key from a kid the node does not trust is invalid there',
    async () => {
      const key = await signer.sign(claimsFor('lic_ORACLEtest02', keys[0]!.terms, signedAt));
      const forged = key.replace(/^[^.]+/, Buffer.from('{"alg":"EdDSA","kid":"kudurru-2099-1","typ":"JWT"}').toString('base64url'));
      const [r] = await nodeStatus([{ key: forged, now: Math.floor(signedAt.getTime() / 1000) }]);
      expect(r!.state).toBe('invalid');
      expect(r!.setError).toBe('license key is signed with a key this build does not trust');
    },
    180_000,
  );
});
