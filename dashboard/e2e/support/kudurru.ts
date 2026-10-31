// Runs the real `kudurru serve` for the Playwright suite: locates (or
// builds) the binary, writes a node configuration with free loopback
// ports under a temporary directory (sqlite store, own data directory,
// admin socket, encryption key and admin password files, objects.d with a
// primary zone, a block list and, on a node with a key, a policy), starts
// the process, waits for fixed-seed metadata, DNS and ops readiness, and
// stops it again (the server log goes to test-results/; KUDURRU_E2E_KEEP=1 keeps
// the temporary directory). The Go side of the same idea is
// internal/testutil/kudurrutest.
import { spawn, spawnSync, type ChildProcess } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import fs from 'node:fs';
import net from 'node:net';
import dgram from 'node:dgram';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { startupRoutesSettled } from './startup';
import { query, TYPE_A } from './dns';

export const WEB_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
export const REPO_DIR = path.resolve(WEB_DIR, '..');

/** The DNSZone of the run: its object name has dots (deep-link test). */
export const ZONE = { object: 'lan.example', apex: 'lan.example.', host: 'nas.lan.example', hostA: '10.0.0.20' };
/** The block list of the run and one name on it. */
export const BLOCKLIST = { name: 'e2e-ads', blocked: 'ads.example.net', alsoBlocked: 'tracker.example.org' };
export const ADMIN_USER = 'admin';
/** The signed key of the e2e node: all features, 10 nodes, no expiry in sight. */
export const E2E_LICENSE_KEY = path.join(REPO_DIR, 'internal', 'license', 'testdata', 'e2e-license.key');

const OBJECTS = `apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: ${ZONE.object} }
spec:
  zoneName: ${ZONE.apex}
  type: primary
  soa: { mname: ns1.${ZONE.apex}, rname: hostmaster.${ZONE.apex} }
  nameservers: [ns1.${ZONE.apex}]
---
apiVersion: kudurru.io/v1alpha1
kind: DNSRecordSet
metadata: { name: hosts, namespace: default }
spec:
  zoneRef: { name: ${ZONE.object} }
  endpoints:
    - { dnsName: ns1.${ZONE.object}, recordType: A, targets: ["127.0.0.1"] }
    - { dnsName: ${ZONE.host}, recordType: A, targets: ["${ZONE.hostA}"], recordTTL: 300 }
    - { dnsName: www.${ZONE.object}, recordType: CNAME, targets: [${ZONE.host}] }
---
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: { name: ${BLOCKLIST.name}, namespace: default }
spec:
  mode: block
  sources:
    - inline: ["||${BLOCKLIST.blocked}^", "${BLOCKLIST.alsoBlocked}"]
      format: adblock
`;

/** The policy of the run, beside the seeded default (not on a node without a key). */
const POLICY = `---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: { name: e2e-blocking, namespace: default }
spec:
  clientGroupRefs: [default]
  blockListRefs: [${BLOCKLIST.name}]
  priority: 10
`;

/** Locates the kudurru binary: KUDURRU_BIN, else ../bin/kudurru (built when missing). */
export function locateBinary(): string {
  const env = process.env.KUDURRU_BIN;
  if (env) {
    const p = path.resolve(WEB_DIR, env);
    if (!fs.existsSync(p)) throw new Error(`KUDURRU_BIN=${env}: no such file`);
    return p;
  }
  const p = path.join(REPO_DIR, 'bin', 'kudurru');
  if (!fs.existsSync(p)) {
    if (!fs.existsSync(path.join(WEB_DIR, 'dist', 'assets'))) {
      throw new Error('no bin/kudurru and no built dashboard in dashboard/dist: run `make dashboard-e2e` (or `make dashboard-build build`) first');
    }
    console.log(`e2e: building ${p}`);
    const r = spawnSync('go', ['build', '-trimpath', '-tags', 'licensetest', '-o', p, './cmd/kudurru'], {
      cwd: REPO_DIR,
      env: { ...process.env, CGO_ENABLED: '0' },
      stdio: 'inherit',
    });
    if (r.status !== 0) throw new Error(`go build failed (${r.error?.message ?? `exit ${r.status}`})`);
  }
  return p;
}

// Ports this process has handed out: the OS may return a just-closed port
// again, which would give two listeners of one node the same address.
const handedOut = new Set<number>();

/** A loopback port free for both TCP and UDP right now. */
export async function freePort(): Promise<number> {
  for (let i = 0; i < 50; i++) {
    const port = await new Promise<number>((resolve, reject) => {
      const srv = net.createServer();
      srv.once('error', reject);
      srv.listen(0, '127.0.0.1', () => {
        const a = srv.address();
        srv.close(() => resolve(typeof a === 'object' && a ? a.port : 0));
      });
    });
    const udpFree = await new Promise<boolean>((resolve) => {
      const s = dgram.createSocket('udp4');
      s.once('error', () => resolve(false));
      s.bind(port, '127.0.0.1', () => s.close(() => resolve(true)));
    });
    if (port && udpFree && !handedOut.has(port)) {
      handedOut.add(port);
      return port;
    }
  }
  throw new Error('no loopback port free for tcp and udp');
}

export interface ServerOptions {
  /** Upstream server ("host:port") of the default group. */
  upstream: string;
  /** The admin password; undefined lets kudurru generate one (must change at first login). */
  adminPassword?: string;
  /** Optional API/dashboard path prefix, without a trailing slash. */
  pathPrefix?: string;
  /** Node name. */
  node?: string;
  /**
   * Run without a license key (the community license). The node starts like
   * a fresh install: the seeded default is its only policy, so it is at the
   * free limit of one policy.
   */
  community?: boolean;
}

export interface Server {
  node: string;
  dir: string;
  /** https://127.0.0.1:<api port> (self-signed certificate). */
  url: string;
  /** http://127.0.0.1:<port> of the block page listener. */
  blockPage: string;
  dns: string;
  ops: string;
  /** The admin password: the configured one, or the generated one from stderr. */
  adminPassword: string;
  log: () => string;
  stop: () => Promise<void>;
}

/**
 * Writes the configuration and starts `kudurru serve`; resolves once it is ready.
 * A port picked by freePort can be taken by another worker before the node binds
 * it, so a start that dies on "address already in use" is retried with new ports.
 */
export async function startServer(bin: string, opts: ServerOptions): Promise<Server> {
  for (let attempt = 1; ; attempt++) {
    try {
      return await startServerOnce(bin, opts);
    } catch (err) {
      if (attempt >= 3 || !/address already in use/.test((err as Error).message)) throw err;
    }
  }
}

async function startServerOnce(bin: string, opts: ServerOptions): Promise<Server> {
  const node = opts.node ?? 'e2e';
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), `kudurru-e2e-${node}-`));
  const dataDir = path.join(dir, 'data');
  const objectsDir = path.join(dir, 'objects.d');
  fs.mkdirSync(dataDir, { recursive: true, mode: 0o750 });
  fs.mkdirSync(objectsDir, { recursive: true });
  fs.writeFileSync(path.join(objectsDir, 'e2e.yaml'), opts.community ? OBJECTS : OBJECTS + POLICY);

  const keyFile = path.join(dir, 'encryption.key');
  fs.writeFileSync(keyFile, randomBytes(32).toString('base64'), { mode: 0o600 });
  const env: Record<string, string> = {};
  for (const [k, v] of Object.entries(process.env)) {
    // A developer's KUDURRU_* settings must not leak into the test node.
    if (v !== undefined && !k.startsWith('KUDURRU_')) env[k] = v;
  }
  env.KUDURRU_ENCRYPTION_KEY_FILE = keyFile;
  // Every licensed feature is on, with a throwaway test key that only a binary
  // built with -tags licensetest trusts (make dashboard-e2e; internal/license/testkey.go).
  // ServerOptions.community runs a node without a key instead.
  if (!opts.community) env.KUDURRU_LICENSE_KEY_FILE = E2E_LICENSE_KEY;
  if (opts.adminPassword !== undefined) {
    const pwFile = path.join(dir, 'admin.password');
    fs.writeFileSync(pwFile, opts.adminPassword, { mode: 0o600 });
    env.KUDURRU_ADMIN_PASSWORD_FILE = pwFile;
  }

  const addr = async () => `127.0.0.1:${await freePort()}`;
  const ports = { dns: await addr(), api: await addr(), ops: await addr(), metrics: await addr(), internal: await addr(), blockPage: await addr() };
  // JSON is YAML: no YAML dependency needed for the configuration.
  const config = {
    node: { name: node, dataDir },
    objectsDir,
    store: { driver: 'sqlite' },
    listeners: {
      dns: [{ address: ports.dns, protocols: ['udp', 'tcp'] }],
      api: { address: ports.api, pathPrefix: opts.pathPrefix },
      ops: { address: ports.ops },
      metrics: { address: ports.metrics },
      internal: { address: ports.internal },
      sockets: 1,
      // The block page listener (requests.spec.ts posts its form); the
      // block response stays zeroIP, so the other specs see 0.0.0.0.
      blockPage: { http: ports.blockPage },
    },
    blockPage: { enabled: true, ipv4: '127.0.0.1' },
    upstreams: { groups: { default: { servers: [opts.upstream], strategy: 'parallel_best', timeout: '1s' } } },
    // Ready only once every referenced block list is compiled; url sources
    // may name the loopback stub publisher (support/lists.ts).
    blocking: { initStrategy: 'blocking', allowLocalListURLs: true },
    auth: { adminSocket: { path: path.join(dir, 'admin.sock') } },
    log: { level: 'info', format: 'text' },
  };
  const configFile = path.join(dir, 'kudurru.yaml');
  fs.writeFileSync(configFile, JSON.stringify(config, null, 2), { mode: 0o600 });

  let out = '';
  const child: ChildProcess = spawn(bin, ['serve', '--config', configFile], { env, stdio: ['ignore', 'pipe', 'pipe'] });
  child.stdout?.on('data', (b: Buffer) => (out += b.toString()));
  child.stderr?.on('data', (b: Buffer) => (out += b.toString()));
  let exited: string | null = null;
  const done = new Promise<void>((resolve) =>
    child.once('exit', (code, sig) => {
      exited = sig ? `signal ${sig}` : `exit ${code}`;
      resolve();
    }),
  );

  const stop = async () => {
    if (exited === null) {
      child.kill('SIGTERM');
      const killed = await Promise.race([done.then(() => false), sleep(15_000).then(() => true)]);
      if (killed) {
        child.kill('SIGKILL');
        await done;
      }
    }
    // Kept next to the traces (CI uploads test-results/ on failure).
    const logDir = path.join(WEB_DIR, 'test-results');
    fs.mkdirSync(logDir, { recursive: true });
    // Concurrent beforeAll nodes can share a name; retain each process log.
    fs.writeFileSync(path.join(logDir, `${path.basename(dir)}.log`), out);
    if (!process.env.KUDURRU_E2E_KEEP) fs.rmSync(dir, { recursive: true, force: true });
  };

  const deadline = Date.now() + 60_000;
  let last: string;
  const remaining = (limit: number) => Math.max(1, Math.min(limit, deadline - Date.now()));
  for (;;) {
    if (exited !== null) {
      if (!process.env.KUDURRU_E2E_KEEP) fs.rmSync(dir, { recursive: true, force: true });
      throw new Error(`kudurru exited before it was ready (${exited as string}):\n${out}`);
    }
    try {
      const r = await fetch(`http://${ports.ops}/healthz/ready`, { signal: AbortSignal.timeout(remaining(2000)) });
      if (r.ok && startupRoutesSettled(path.join(dataDir, 'state.db'), ZONE.object)) {
        // Check fixed-seed DNS membership and current publication after
        // metadata settles. A later command still retains its own gates.
        const answer = await query(ports.dns, ZONE.host, TYPE_A, remaining(3000));
        const published = await fetch(`http://${ports.ops}/healthz/ready`, { signal: AbortSignal.timeout(remaining(2000)) });
        if (answer.rcode === 0 && answer.a.includes(ZONE.hostA) && published.ok && Date.now() <= deadline) break;
      }
      last = r.ok ? 'startup route status/lifecycle publication pending' : `${r.status} ${(await r.text()).trim()}`;
    } catch (err) {
      last = (err as Error).message;
    }
    if (Date.now() > deadline) {
      await stop();
      throw new Error(`kudurru not ready after 60 s: ${last}\n${out}`);
    }
    await sleep(100);
  }

  let adminPassword = opts.adminPassword;
  if (adminPassword === undefined) {
    const m = /generated password:\s+(\S+)/.exec(out);
    if (!m?.[1]) {
      await stop();
      throw new Error(`kudurru printed no generated admin password:\n${out}`);
    }
    adminPassword = m[1];
  }
  return { node, dir, url: `https://${ports.api}${opts.pathPrefix ?? ''}`, blockPage: `http://${ports.blockPage}`, dns: ports.dns, ops: ports.ops, adminPassword, log: () => out, stop };
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
