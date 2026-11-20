import { createHash, randomBytes } from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import { createInterface } from 'node:readline/promises';
import { query, startUpstream } from '../dashboard/e2e/support/dns';

const day = 86_400;
const featureNames = ['audit', 'branding', 'edge', 'integrations', 'longterm', 'oidc', 'rbac', 'requests'];
const zone = 'license-check.example';
const host = `test.${zone}`;
const hostAddress = '192.0.2.20';
const brand = 'Disposable Acceptance';
const repo = fs.realpathSync(path.join(import.meta.dir, '..'));
const checks: { check: string; observedAt: string }[] = [];
const interrupted = new AbortController();
let stage = 'arguments';

function requireCheck(ok: unknown): asserts ok {
  if (!ok) throw new Error('check failed');
}

function record(check: string) {
  checks.push({ check, observedAt: new Date().toISOString() });
}

function outsideCheckout(file: string) {
  requireCheck(file !== repo && !file.startsWith(repo + path.sep));
  for (let parent = path.dirname(file); parent !== path.dirname(parent); parent = path.dirname(parent)) {
    requireCheck(!fs.existsSync(path.join(parent, '.git')));
  }
}

function readClaims(input: string) {
  requireCheck(path.isAbsolute(input));
  outsideCheckout(path.normalize(input));
  const file = fs.realpathSync(input);
  outsideCheckout(file);
  const parent = fs.statSync(path.dirname(file));
  requireCheck(parent.uid === process.getuid?.() && (parent.mode & 0o077) === 0);
  const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const stat = fs.fstatSync(fd);
    requireCheck(stat.isFile() && stat.uid === process.getuid?.() && stat.nlink === 1 && (stat.mode & 0o077) === 0);
    requireCheck(stat.size > 0 && stat.size <= 16_384);
    const parts = fs.readFileSync(fd, 'utf8').trim().split('.');
    requireCheck(parts.length === 3 && parts.every((part) => /^[A-Za-z0-9_-]+$/.test(part)));
    const { exp, plan, nodes, features } = JSON.parse(Buffer.from(parts[1], 'base64url').toString('utf8'));
    requireCheck(Number.isSafeInteger(exp) && exp > 0 && Number.isSafeInteger(nodes) && nodes > 0);
    requireCheck(['personal', 'business', 'enterprise', 'trial'].includes(plan));
    requireCheck(features === undefined || (Array.isArray(features) && features.every((name) => typeof name === 'string')));
    const expected = featureNames.filter((name) => (name !== 'branding' || plan === 'enterprise') &&
      (!features?.length || features.includes(name)));
    return { file, exp: exp as number, plan: plan as string, nodes: nodes as number, features: expected };
  } finally {
    fs.closeSync(fd);
  }
}

async function waitFor(check: () => Promise<boolean>) {
  const deadline = performance.now() + 60_000;
  while (performance.now() < deadline) {
    if (interrupted.signal.aborted) throw new Error('interrupted');
    if (await check().catch(() => false)) return;
    await Bun.sleep(250);
  }
  throw new Error('timed out');
}

async function loopbackAddresses() {
  const sockets: net.Server[] = [];
  try {
    for (let index = 0; index < 5; index++) {
      const socket = net.createServer();
      sockets.push(socket);
      await new Promise<void>((resolve, reject) => {
        socket.once('error', reject).listen(0, '127.0.0.1', resolve);
      });
    }
    return sockets.map((socket) => `127.0.0.1:${(socket.address() as net.AddressInfo).port}`);
  } finally {
    await Promise.all(sockets.map((socket) => new Promise((resolve) => socket.close(resolve))));
  }
}

async function pauseAt(phase: string, target: number) {
  const terminal = createInterface({ input: process.stdin, output: process.stderr });
  const closed = new AbortController();
  terminal.once('close', () => closed.abort());
  try {
    await terminal.question(`In a second console of this disposable VM, set UTC to ${new Date(target * 1000).toISOString()} ` +
      `(Unix ${target}) for ${phase}. Then press Enter here.\n`, { signal: AbortSignal.any([interrupted.signal, closed.signal]) });
    const now = Math.floor(Date.now() / 1000);
    requireCheck(now >= target && now < target + 3600);
  } finally {
    terminal.close();
  }
}

type Claims = ReturnType<typeof readClaims>;

async function runNode(binary: string, work: string, claims: Claims | undefined, sequence: boolean) {
  stage = 'fixture setup';
  fs.mkdirSync(work, { mode: 0o700 });
  const objectsDir = path.join(work, 'objects');
  fs.mkdirSync(objectsDir, { mode: 0o700 });
  fs.writeFileSync(path.join(objectsDir, 'zone.yaml'), `apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: ${zone} }
spec:
  zoneName: ${zone}.
  type: primary
  soa: { mname: ns1.${zone}., rname: hostmaster.${zone}. }
  nameservers: [ns1.${zone}.]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: hosts, namespace: default }
spec:
  zoneRef: { name: ${zone} }
  endpoints:
    - { dnsName: ${host}, recordType: A, targets: ["${hostAddress}"] }
`, { mode: 0o600 });
  const password = randomBytes(24).toString('hex');
  const passwordFile = path.join(work, 'password');
  const encryptionFile = path.join(work, 'encryption');
  const configFile = path.join(work, 'config.yaml');
  fs.writeFileSync(passwordFile, password, { mode: 0o600 });
  fs.writeFileSync(encryptionFile, randomBytes(32).toString('base64'), { mode: 0o600 });
  const [dns, api, ops, metrics, internal] = await loopbackAddresses();
  const upstream = await startUpstream();
  let child: { kill: (signal: NodeJS.Signals) => void; exited: Promise<number> } | undefined;
  const stop = async () => {
    if (!child) return;
    const current = child;
    current.kill('SIGTERM');
    const timer = setTimeout(() => current.kill('SIGKILL'), 10_000);
    try {
      await current.exited;
    } finally {
      clearTimeout(timer);
      child = undefined;
    }
  };
  const request = (route: string, options: RequestInit = {}) => fetch(`https://${api}/api/v1${route}`, {
    ...options, redirect: 'error', proxy: '', tls: { rejectUnauthorized: false }, signal: AbortSignal.timeout(5_000),
  });
  const start = async () => {
    child = Bun.spawn([binary, 'serve', '--config', configFile], {
      cwd: work, stdin: 'ignore', stdout: 'ignore', stderr: 'ignore',
      env: {
        HOME: work, TMPDIR: work,
        KUDURRU_ENCRYPTION_KEY_FILE: encryptionFile,
        KUDURRU_ADMIN_PASSWORD_FILE: passwordFile,
        ...(claims ? { KUDURRU_LICENSE_KEY_FILE: claims.file } : {}),
      },
    });
    await waitFor(async () => {
      const ready = await fetch(`http://${ops}/healthz/ready`, { proxy: '', signal: AbortSignal.timeout(1_000) });
      return ready.ok && await dnsAnswers();
    });
  };
  const dnsAnswers = async () => {
    const answer = await query(dns, host);
    return answer.rcode === 0 && answer.a.includes(hostAddress);
  };
  const checkState = async (state: 'active' | 'grace' | 'expired' | 'community', firstStart = false) => {
    stage = `${state}: login`;
    const login = await request('/auth/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ username: 'admin', password }),
    });
    requireCheck(login.ok);
    const cookies = login.headers.getSetCookie().map((cookie) => cookie.split(';', 1)[0]).join('; ');
    const { csrf } = await login.json() as { csrf?: string };
    requireCheck(cookies && csrf);
    const headers = { Cookie: cookies, 'X-Kudurru-CSRF': csrf };
    const licensed = state === 'active' || state === 'grace';
    const expected = licensed ? claims!.features : [];
    stage = `${state}: effective license`;
    await waitFor(async () => {
      const result = await request('/license', { headers });
      if (!result.ok) return false;
      const status = await result.json() as { state: string; plan?: string; nodes: number; policyLimit: number;
        features: string[]; expiresAt?: string; graceEndsAt?: string };
      return status.state === state && status.nodes === (licensed ? claims!.nodes : 1) &&
        status.policyLimit === (licensed ? 0 : 1) && JSON.stringify([...status.features].sort()) === JSON.stringify(expected) &&
        (!licensed || status.plan === claims!.plan) && (claims ?
          Date.parse(status.expiresAt ?? '') === claims.exp * 1000 && Date.parse(status.graceEndsAt ?? '') === (claims.exp + 30 * day) * 1000 :
          status.expiresAt === undefined && status.graceEndsAt === undefined);
    });
    stage = `${state}: audit gate`;
    requireCheck((await request('/audit', { headers })).status === (expected.includes('audit') ? 200 : 402));
    stage = `${state}: policy gate`;
    if (claims && !firstStart) {
      requireCheck((await request('/objects/policies/default/license-check-active', { headers })).ok);
    }
    if (firstStart || state !== 'active') {
      const name = `license-check-${state}`;
      const policy = await request(`/objects/policies/default/${name}`, { method: 'PUT',
        headers: { ...headers, 'Content-Type': 'application/json' },
        body: JSON.stringify({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name },
          spec: { clientGroupRefs: ['default'] } }),
      });
      requireCheck(licensed ? policy.ok : policy.status === 402);
    }
    stage = `${state}: branding`;
    if (firstStart && expected.includes('branding')) {
      const changed = await request('/settings', { method: 'PATCH',
        headers: { ...headers, 'Content-Type': 'application/merge-patch+json' },
        body: JSON.stringify({ branding: { name: brand, accentColor: '#0064E0' } }),
      });
      requireCheck(changed.ok);
    }
    await waitFor(async () => {
      const result = await request('/branding');
      if (!result.ok) return false;
      const effective = await result.json() as { enabled: boolean; name: string };
      return effective.enabled === expected.includes('branding') && effective.name === (effective.enabled ? brand : 'kudurru');
    });
    stage = `${state}: authoritative DNS`;
    requireCheck(await dnsAnswers());
  };
  try {
    fs.writeFileSync(configFile, JSON.stringify({
      node: { name: 'license-check', dataDir: path.join(work, 'data') }, objectsDir, store: { driver: 'sqlite' },
      listeners: { dns: [{ address: dns, protocols: ['udp', 'tcp'] }], api: { address: api }, ops: { address: ops },
        metrics: { address: metrics }, internal: { address: internal }, sockets: 1 },
      upstreams: { groups: { default: { servers: [upstream.address], strategy: 'parallel_best', timeout: '1s' } } },
      auth: { adminSocket: { path: path.join(work, 'admin.sock') } },
    }), { mode: 0o600 });
    stage = 'archive startup';
    await start();
    if (!claims) {
      await checkState('community');
      record('clean Community startup, default branding, audit and extra policy refused, DNS answers');
      return;
    }
    await checkState('active', true);
    record('active signed entitlement, branding, audit gate, extra policy allowed, DNS answers');
    stage = 'archive restart';
    await stop();
    await start();
    await checkState('active');
    record('active entitlement, branding, and policy persist across restart; DNS answers');
    if (sequence) {
      stage = 'grace clock confirmation';
      await pauseAt('grace', claims.exp + day);
      await checkState('grace');
      record('grace on running node, same entitlements and branding, extra policy allowed, DNS answers');
      stage = 'expired clock confirmation';
      await pauseAt('expired', claims.exp + 31 * day);
      await checkState('expired');
      record('expired on running node, Community limits and default branding, audit and new policy refused, existing policy retained, DNS answers');
    }
  } finally {
    await stop();
    await upstream.close();
  }
}

async function main() {
  const [mode, binaryInput, expectedSha, keyFile, ...extra] = process.argv.slice(2);
  requireCheck(['sequence', 'active', 'community'].includes(mode) && Boolean(binaryInput) && /^[0-9a-f]{64}$/.test(expectedSha) &&
    extra.length === 0 && (mode === 'community' ? keyFile === undefined : Boolean(keyFile)));
  requireCheck(mode !== 'sequence' || process.stdin.isTTY);
  stage = 'binary identity';
  const binary = fs.realpathSync(binaryInput);
  requireCheck(fs.statSync(binary).isFile());
  const hash = createHash('sha256');
  for await (const bytes of fs.createReadStream(binary)) hash.update(bytes);
  const binarySha256 = hash.digest('hex');
  requireCheck(binarySha256 === expectedSha);
  stage = 'private key input';
  const claims = keyFile ? readClaims(keyFile) : undefined;
  requireCheck(mode !== 'sequence' || (claims?.features.includes('branding') && claims.features.includes('audit')));
  stage = 'private temporary directory';
  const temp = fs.realpathSync('/tmp');
  outsideCheckout(path.join(temp, 'placeholder'));
  const work = fs.mkdtempSync(path.join(temp, 'kudurru-license-check-'));
  try {
    if (claims) await runNode(binary, path.join(work, 'licensed'), claims, mode === 'sequence');
    await runNode(binary, path.join(work, 'community'), undefined, false);
  } finally {
    stage = interrupted.signal.aborted ? 'interrupted' : stage;
    fs.rmSync(work, { recursive: true, force: true });
  }
  requireCheck(!interrupted.signal.aborted);
  console.log(JSON.stringify({ binarySha256, mode, checks, passed: true, humanVisualApproval: false,
    ...(claims ? { expiresAt: new Date(claims.exp * 1000).toISOString(), graceEndsAt: new Date((claims.exp + 30 * day) * 1000).toISOString() } : {}),
  }, null, 2));
}

for (const signal of ['SIGINT', 'SIGTERM'] as const) process.on(signal, () => interrupted.abort());
try {
  await main();
} catch {
  console.error(`License check did not pass (${stage}). No server output or response bodies were retained.`);
  if (stage === 'arguments') console.error('Usage: bun hack/production-license-check.ts <sequence|active|community> <binary> <binary-sha256> [private-key-file]');
  process.exitCode = 1;
}
