// MSW handlers for the integration administration routes (internal/api
// integrations.go, D367, D400) over a small in-memory state: the manifests
// of the three providers of this build (the configuration schemas of
// ee/integration/*/manifest.go, verbatim: lib/integrations.test.ts
// compares them), Integration objects written through
// /objects/integrations with references only, the host's status of each
// exact revision, and the reviewed commands. Tests change the state with
// mockIntegrations() (license, driver, a stale preview, an uncertain sync).
import { http, HttpResponse, type JsonBodyType } from 'msw';
import type { Integration, IntegrationExecutionResult, IntegrationManifest, IntegrationManifests, IntegrationReview, IntegrationSummary } from '../api/types';

const CLOUDFLARE_SCHEMA = {"type":"object","required":["zones"],"additionalProperties":false,"properties":{"zones":{"type":"array","minItems":1,"maxItems":256,"items":{"type":"object","required":["zone"],"additionalProperties":false,"properties":{"zone":{"type":"string","maxLength":253},"zoneID":{"type":"string","pattern":"^[a-f0-9]{32}$"},"policy":{"type":"string","enum":["sync","upsert-only","create-only"]},"adoptUnowned":{"type":"boolean"},"adoptFrom":{"type":"object","required":["externalDNSOwner"],"additionalProperties":false,"properties":{"externalDNSOwner":{"type":"string","minLength":1,"maxLength":253}}},"defaults":{"type":"object","additionalProperties":false,"properties":{"proxied":{"type":"boolean"},"ttl":{"type":"integer","minimum":1,"maximum":86400},"comment":{"type":"string","maxLength":500}}},"select":{"type":"object","additionalProperties":false,"properties":{"labels":{"type":"object","additionalProperties":{"type":"string"},"maxProperties":64},"types":{"type":"array","maxItems":64,"items":{"type":"string"}},"exclude":{"type":"array","maxItems":256,"items":{"type":"string"}}}},"translate":{"type":"object","maxProperties":64,"additionalProperties":{"type":"string"}}}}}}};
const UNIFI_SCHEMA = {"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048,"pattern":"^https://[^/]+.*$"},"tlsFingerprint":{"type":"string","pattern":"^sha256:[a-fA-F0-9]{64}$"},"site":{"type":"string","minLength":1,"maxLength":253},"poll":{"type":"string"},"legacy":{"type":"boolean"},"inventory":{"type":"object","required":["zoneRef"],"additionalProperties":false,"properties":{"zoneRef":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"},"namespace":{"type":"string"}}},"reverseZones":{"type":"string","enum":["auto","disabled"]},"nameTemplate":{"type":"string","maxLength":500},"ttl":{"type":"integer","minimum":1,"maximum":86400},"includeNetworks":{"type":"array","maxItems":256,"items":{"type":"string"},"uniqueItems":true}}},"groups":{"type":"object","additionalProperties":false,"properties":{"byNetwork":{"type":"object","maxProperties":256,"additionalProperties":{"type":"string"}},"byNote":{"type":"string","maxLength":128}}},"push":{"type":"object","additionalProperties":false,"properties":{"dhcpDNSServers":{"type":"boolean"},"dnsServers":{"type":"array","maxItems":4,"uniqueItems":true,"items":{"type":"string","format":"ipv4"}},"networks":{"type":"array","maxItems":64,"uniqueItems":true,"items":{"type":"string","minLength":1,"maxLength":253}},"clientActions":{"type":"boolean"},"mirrorRecords":{"type":"object","additionalProperties":false,"properties":{"enabled":{"type":"boolean"},"types":{"type":"array","items":{"type":"string","enum":["A","CNAME"]},"maxItems":2,"uniqueItems":true},"domains":{"type":"array","maxItems":64,"uniqueItems":true,"items":{"type":"string","maxLength":253}},"labels":{"type":"object","maxProperties":64,"additionalProperties":{"type":"string"}},"exclude":{"type":"array","maxItems":256,"items":{"type":"string","maxLength":253}}}}}}}};
const WEBHOOK_SCHEMA = {"type":"object","required":["endpoint"],"additionalProperties":false,"properties":{"endpoint":{"type":"string","maxLength":2048},"timeout":{"type":"string"},"domains":{"type":"array","maxItems":256,"items":{"type":"string","maxLength":253}},"policy":{"type":"string","enum":["sync","upsert-only","create-only"]}}};

// cmd/kudurru integration_manifests.go: the SQLite root refuses nothing
// (it runs every UniFi write, D401); the Kubernetes root refuses UniFi
// inventory publication and every UniFi write, and every webhook.
const KUBE_UNIFI_REFUSED = ['spec.unifi.inventory', 'spec.unifi.push.dhcpDNSServers', 'spec.unifi.push.mirrorRecords', 'spec.unifi.push.clientActions'];

function manifests(driver: 'sqlite' | 'kube'): IntegrationManifest[] {
  return [
    { type: 'cloudflare', displayName: 'Cloudflare', version: '1', capabilities: ['RecordSink'], ioClass: 'cloud_polling', configField: 'cloudflare', configSchema: CLOUDFLARE_SCHEMA, credentials: [{ name: 'apiToken', required: true, verify: 'user/tokens/verify' }], supported: true, refused: [] },
    { type: 'unifi', displayName: 'UniFi Network', version: '1', capabilities: ['ClientSource', 'RecordSink', 'Actions'], ioClass: 'local_polling', configField: 'unifi', configSchema: UNIFI_SCHEMA, credentials: [{ name: 'apiKey', required: true, verify: 'integration/v1/info' }], supported: true, refused: driver === 'kube' ? [...KUBE_UNIFI_REFUSED] : [] },
    {
      type: 'webhook', displayName: 'Webhook', version: '1', capabilities: ['Actions', 'ClientSource', 'Hook', 'RecordSink'], ioClass: 'local_polling', configField: 'webhook', configSchema: WEBHOOK_SCHEMA,
      credentials: ['bearerToken', 'ca', 'clientCertificate', 'clientKey', 'hmacKey'].map((name) => ({ name, required: false })),
      supported: driver !== 'kube', ...(driver === 'kube' ? { reason: 'UnsupportedOnKubernetes' } : {}), refused: [],
    },
  ];
}

/** The manifests the mock serves for a store driver (tests compare them with the Go manifests). */
export const mockManifests = manifests;

export interface IntegrationsMock {
  driver: 'sqlite' | 'kube';
  licensed: boolean;
  /** No integration host: every route answers 503 unavailable. */
  unavailable: boolean;
  credentialSources: string[];
  allowedFiles: string[];
  allowedEnvironment: string[];
  objects: Integration[];
  status: Record<string, Partial<IntegrationSummary>>;
  /** The digest of the last preview per instance; a sync must send it back. */
  digests: Record<string, string>;
  /** The next sync answers 409 as an expired preview. */
  expireNextPreview: boolean;
  /** The next sync answers 503 integration_uncertain. */
  uncertainNextSync: boolean;
  /** What the next Checks report (both true unless a test sets them). */
  checkResult: { reachable: boolean; authenticated: boolean };
  /** Request bodies of the commands, newest last (tests assert on them). */
  calls: { path: string; body: unknown }[];
  revision: number;
}

let n = 0;
function fixture(): IntegrationsMock {
  return {
    driver: 'sqlite', licensed: true, unavailable: false, credentialSources: ['file', 'env'],
    allowedFiles: ['/run/secrets/cloudflare-token', '/run/secrets/unifi-key', '/run/secrets/webhook-token'], allowedEnvironment: ['CLOUDFLARE_API_TOKEN'],
    objects: [
      {
        apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration',
        metadata: { name: 'public-dns', namespace: 'default', uid: 'uid-public-dns', resourceVersion: '10', annotations: { 'kudurru.io/managed-by': 'api' } },
        spec: { type: 'cloudflare', credentials: { apiToken: { env: 'CLOUDFLARE_API_TOKEN' } }, cloudflare: { zones: [{ zone: 'example.org', policy: 'sync' }] }, deletionPolicy: 'Delete' },
        status: { records: { managed: 4, drift: 0, conflicts: 1 }, conditions: [{ type: 'Ready', status: 'True', reason: 'Synced', message: 'published 4 records' }], lastSyncTime: new Date(Date.now() - 60_000).toISOString() },
      },
      {
        apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration',
        metadata: { name: 'office', namespace: 'default', uid: 'uid-office', resourceVersion: '11', annotations: { 'kudurru.io/managed-by': 'file:objects.d/unifi.yaml' } },
        spec: { type: 'unifi', credentials: { apiKey: { file: '/run/secrets/unifi-key' } }, unifi: { endpoint: 'https://unifi.lan', site: 'default' } },
        status: { clients: 23 },
      },
    ],
    status: {}, digests: {}, expireNextPreview: false, uncertainNextSync: false, checkResult: { reachable: true, authenticated: true }, calls: [], revision: 20,
  };
}

let state = fixture();
export const mockIntegrations = () => state;
export const resetIntegrations = () => {
  state = fixture();
  n = 0;
};

type Guard = (r: Request, resource: string, verb: string) => Response | null;
type Allowed = (resource: string, verb: string) => boolean;

const fail = (status: number, key: string, message: string, hint?: unknown) =>
  HttpResponse.json({ error: { key, message, ...(hint !== undefined ? { hint } : {}) }, took_ms: 1 } as JsonBodyType, { status });

const keyOf = (ns: string, name: string) => `${ns}/${name}`;

function find(ns: string, name: string): Integration | undefined {
  return state.objects.find((o) => (o.metadata.namespace ?? 'default') === ns && o.metadata.name === name);
}

function summary(o: Integration): IntegrationSummary {
  const ns = o.metadata.namespace ?? 'default';
  const terminating = !!o.metadata.deletionTimestamp;
  const base: IntegrationSummary = {
    namespace: ns, name: o.metadata.name, uid: o.metadata.uid ?? '', revision: o.metadata.resourceVersion ?? '', type: o.spec.type, suspended: o.spec.suspend === true,
    deletionPolicy: o.spec.deletionPolicy === 'Delete' ? 'Delete' : 'Retain', terminating, remaining: 0, blocked: 0, uncertain: 0,
    statusKnown: true, phase: !state.licensed ? 'Paused' : terminating ? 'Terminating' : 'Ready', reason: !state.licensed ? 'LicenseUnavailable' : terminating ? 'CleanupInProgress' : '', leader: true,
    ...(o.status?.lastSyncTime ? { lastSync: o.status.lastSyncTime } : {}),
    ...(terminating && o.metadata.deletionTimestamp ? { deletionRequestedAt: o.metadata.deletionTimestamp, remaining: 3, blocked: 1 } : {}),
  };
  return { ...base, ...state.status[keyOf(ns, o.metadata.name)] };
}

/** cmd/kudurru integrationPublishes. */
function publishes(o: Integration): boolean {
  if (o.spec.type === 'unifi') return (o.spec.unifi as { push?: { mirrorRecords?: { enabled?: boolean } } } | undefined)?.push?.mirrorRecords?.enabled === true;
  return o.spec.type === 'cloudflare' || (o.spec.type === 'webhook' && Array.isArray((o.spec.webhook as { domains?: unknown[] } | undefined)?.domains) && ((o.spec.webhook as { domains: unknown[] }).domains.length > 0));
}

/** The actions a UniFi instance advertises (ee/integration/unifi actions.go): only the writes its spec enables. */
function unifiActions(o: Integration) {
  const u = (o.spec.unifi ?? {}) as { legacy?: boolean; push?: { dhcpDNSServers?: boolean; clientActions?: boolean } };
  const mac = { type: 'object', required: ['mac'], additionalProperties: false, properties: { mac: { type: 'string', maxLength: 17, pattern: '^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$' } } };
  return [
    ...(u.push?.dhcpDNSServers ? [{ name: 'dhcp-dns-servers', params: { type: 'object', required: ['mode'], additionalProperties: false, properties: { mode: { type: 'string', enum: ['preview', 'apply'] }, digest: { type: 'string', pattern: '^[a-f0-9]{64}$' } } } }] : []),
    ...(u.push?.clientActions && u.legacy ? [{ name: 'block-client', params: mac }, { name: 'unblock-client', params: mac }] : []),
  ];
}

function hasValue(spec: Integration['spec']): boolean {
  return Object.values(spec.credentials ?? {}).some((r) => Object.keys(r).some((k) => !['file', 'env', 'secretRef'].includes(k)));
}

function credentialsChanged(a: Integration['spec'] | undefined, b: Integration['spec']): boolean {
  return JSON.stringify(a?.credentials ?? {}) !== JSON.stringify(b.credentials ?? {});
}

const result = (r: Partial<IntegrationExecutionResult>): IntegrationExecutionResult => ({ completed: true, uncertain: false, applied: 0, unchanged: 0, pending: 0, conflicts: 0, clients: 0, sourceCommitted: false, sourcePublished: false, ...r });

export function integrationHandlers(guard: Guard, allowed: Allowed) {
  const B = '*/api/v1';
  const host = () => (state.unavailable ? fail(503, 'unavailable', 'complete integration administration unavailable') : null);
  const reviewed = async (request: Request, ns: string, name: string): Promise<{ o: Integration; review: IntegrationReview } | Response> => {
    const o = find(ns, name);
    if (!o) return fail(404, 'not_found', 'integration not found');
    const review = (await request.json()) as IntegrationReview;
    state.calls.push({ path: new URL(request.url).pathname, body: review });
    if (review.uid !== o.metadata.uid || review.revision !== o.metadata.resourceVersion) return fail(409, 'conflict', 'original integration review or authority changed');
    return { o, review };
  };
  const command = (path: string, verb: string, run: (o: Integration, review: IntegrationReview, ns: string, name: string) => Response) =>
    http.post(`${B}/integrations/:ns/:name/${path}`, async ({ request, params }) => {
      const g = guard(request, 'integrations', verb) ?? host();
      if (g) return g;
      const r = await reviewed(request, String(params.ns), String(params.name));
      if (r instanceof Response) return r;
      if (r.o.metadata.deletionTimestamp && path !== 'release') return fail(409, 'conflict', 'original integration review or authority changed');
      return run(r.o, r.review, String(params.ns), String(params.name));
    });

  return [
    http.get(`${B}/integrations/manifests`, ({ request }) => {
      const g = guard(request, 'integrations', 'read') ?? host();
      if (g) return g;
      const body: IntegrationManifests = {
        driver: state.driver, licensed: state.licensed, credentialSources: state.credentialSources, items: manifests(state.driver),
        ...(state.driver === 'kube' ? { namespace: 'kudurru' } : {}),
        ...(allowed('integrations', 'secrets') ? { allowedFiles: state.allowedFiles, allowedEnvironment: state.allowedEnvironment } : {}),
      };
      return HttpResponse.json(body);
    }),
    http.get(`${B}/integrations`, ({ request }) => {
      const g = guard(request, 'integrations', 'read') ?? host();
      if (g) return g;
      const ns = new URL(request.url).searchParams.get('namespace');
      if (!ns) return fail(422, 'invalid', 'namespace is required');
      return HttpResponse.json({ items: state.objects.filter((o) => (o.metadata.namespace ?? 'default') === ns).map(summary) });
    }),
    http.get(`${B}/integrations/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'integrations', 'read') ?? host();
      if (g) return g;
      const o = find(String(params.ns), String(params.name));
      return o ? HttpResponse.json(summary(o)) : fail(404, 'not_found', 'integration not found');
    }),
    command('check', 'check', () => HttpResponse.json(result({ ...state.checkResult }))),
    command('preview', 'read', (o, _r, ns, name) => {
      if (!publishes(o)) return fail(501, 'not_implemented', 'integration capability is not implemented');
      const digest = (++n).toString(16).padStart(64, 'a');
      state.digests[keyOf(ns, name)] = digest;
      if (o.spec.type === 'unifi') {
        // The record mirror: DNS policies of the site, one per address.
        return HttpResponse.json(result({
          preview: {
            digest, unchanged: 1,
            operations: [{ domains: ['example.com'], name: 'nas.example.com', type: 'A', action: 'create', policy: 'sync', after: { name: 'nas.example.com', type: 'A', ttl: 300, targets: ['192.0.2.20'] } }],
            conflicts: [{ domains: ['example.com'], name: 'printer.example.com', type: 'A', reason: 'a DNS policy this Integration did not create' }],
          },
        }));
      }
      return HttpResponse.json(result({
        preview: {
          digest, unchanged: 2,
          operations: [{ domains: ['example.org'], name: 'www.example.org', type: 'A', action: 'create', policy: 'sync', after: { name: 'www.example.org', type: 'A', ttl: 300, targets: ['192.0.2.10'] } }],
          conflicts: [{ domains: ['example.org'], name: 'mail.example.org', type: 'MX', reason: 'owned by another writer' }],
        },
      }));
    }),
    command('sync', 'sync', (o, review, ns, name) => {
      const k = keyOf(ns, name);
      if (publishes(o)) {
        if (review.mode !== 'publish' || !review.previewDigest || review.previewDigest.length !== 64) return fail(400, 'invalid', 'invalid integration review or command');
        if (state.expireNextPreview || review.previewDigest !== state.digests[k]) {
          state.expireNextPreview = false;
          delete state.digests[k];
          return fail(409, 'conflict', 'original integration review or authority changed');
        }
      } else if (review.mode !== 'observe' || review.previewDigest) {
        return fail(400, 'invalid', 'invalid integration review or command');
      }
      delete state.digests[k];
      if (state.uncertainNextSync) {
        state.uncertainNextSync = false;
        return fail(503, 'integration_uncertain', 'integration operation was not acknowledged; effects may have occurred and were not replayed', result({ completed: false, uncertain: true, applied: 1, pending: 1 }));
      }
      // A publisher that also observes (UniFi with the record mirror) reports both.
      const observed = { clients: 23, sourceCommitted: true, sourcePublished: true };
      return HttpResponse.json(result(publishes(o) ? { applied: 1, unchanged: 2, conflicts: 1, ...(o.spec.type === 'unifi' ? observed : {}) } : observed));
    }),
    http.get(`${B}/integrations/:ns/:name/actions`, ({ request, params }) => {
      const g = guard(request, 'integrations', 'read') ?? host();
      if (g) return g;
      const o = find(String(params.ns), String(params.name));
      if (!o) return fail(404, 'not_found', 'integration not found');
      const u = new URL(request.url);
      if (u.searchParams.get('uid') !== o.metadata.uid || u.searchParams.get('revision') !== o.metadata.resourceVersion) return fail(409, 'conflict', 'original integration review or authority changed');
      if (o.spec.type === 'unifi') return HttpResponse.json(result({ actions: unifiActions(o) }));
      return HttpResponse.json(result({ actions: [{ name: 'refresh', params: { type: 'object', properties: { full: { type: 'boolean' } } } }] }));
    }),
    http.post(`${B}/integrations/:ns/:name/actions/:action`, async ({ request, params }) => {
      const g = guard(request, 'integrations', 'act') ?? host();
      if (g) return g;
      const r = await reviewed(request, String(params.ns), String(params.name));
      if (r instanceof Response) return r;
      return HttpResponse.json(result({}));
    }),
    command('release', 'release', (o) => {
      if (!o.metadata.deletionTimestamp) return fail(409, 'conflict', 'integration is not terminating');
      state.objects = state.objects.filter((x) => x !== o);
      return HttpResponse.json({ released: true, ledgerRows: 4, operations: 9 });
    }),

    // Integration objects (D400): references only, CAS on resourceVersion.
    http.get(`${B}/objects/integrations`, ({ request }) => guard(request, 'integrations', 'read') ?? HttpResponse.json({ items: state.objects })),
    http.get(`${B}/objects/integrations/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'integrations', 'read');
      if (g) return g;
      const o = find(String(params.ns), String(params.name));
      return o ? HttpResponse.json(o) : fail(404, 'not_found', 'integrations not found');
    }),
    http.post(`${B}/objects/integrations`, async ({ request }) => {
      const g = guard(request, 'integrations', 'write');
      if (g) return g;
      const obj = (await request.json()) as Integration;
      if (hasValue(obj.spec)) return fail(400, 'invalid', 'Integration spec.credentials is invalid', undefined);
      if (credentialsChanged(undefined, obj.spec) && !allowed('integrations', 'secrets')) return fail(403, 'forbidden', 'You may not secrets integrations', { missing: { resource: 'integrations', verb: 'secrets', scope: 'cluster' } });
      if (!state.licensed) return fail(402, 'license_required', 'Integrations needs a license key');
      const ns = obj.metadata.namespace ?? 'default';
      if (find(ns, obj.metadata.name)) return fail(409, 'conflict', `integrations ${ns}/${obj.metadata.name} already exists`);
      const stored: Integration = { ...obj, metadata: { ...obj.metadata, namespace: ns, uid: `uid-${obj.metadata.name}`, resourceVersion: String(++state.revision), annotations: { ...(obj.metadata.annotations ?? {}), 'kudurru.io/managed-by': 'api' } } };
      state.objects.push(stored);
      return HttpResponse.json(stored, { status: 201 });
    }),
    http.put(`${B}/objects/integrations/:ns/:name`, async ({ request, params }) => {
      const g = guard(request, 'integrations', 'write');
      if (g) return g;
      const o = find(String(params.ns), String(params.name));
      if (!o) return fail(404, 'not_found', 'integrations not found');
      const owner = o.metadata.annotations?.['kudurru.io/managed-by'];
      if (owner && owner !== 'api') return fail(409, 'managed_by', `integrations ${o.metadata.name} is managed by ${owner}`, owner);
      if (request.headers.get('If-Match') !== o.metadata.resourceVersion) return fail(412, 'precondition_failed', 'resourceVersion does not match');
      const obj = (await request.json()) as Integration;
      if (hasValue(obj.spec)) return fail(400, 'invalid', 'Integration spec.credentials is invalid');
      if (credentialsChanged(o.spec, obj.spec) && !allowed('integrations', 'secrets')) return fail(403, 'forbidden', 'You may not secrets integrations', { missing: { resource: 'integrations', verb: 'secrets', scope: 'cluster' } });
      const stored: Integration = { ...o, spec: obj.spec, metadata: { ...o.metadata, resourceVersion: String(++state.revision) } };
      state.objects = state.objects.map((x) => (x === o ? stored : x));
      state.calls.push({ path: new URL(request.url).pathname, body: obj });
      return HttpResponse.json(stored);
    }),
    http.delete(`${B}/objects/integrations/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'integrations', 'delete');
      if (g) return g;
      const o = find(String(params.ns), String(params.name));
      if (!o) return fail(404, 'not_found', 'integrations not found');
      const owner = o.metadata.annotations?.['kudurru.io/managed-by'];
      if (owner && owner !== 'api') return fail(409, 'managed_by', `integrations ${o.metadata.name} is managed by ${owner}`, owner);
      const match = request.headers.get('If-Match');
      if (match && match !== o.metadata.resourceVersion) return fail(412, 'precondition_failed', 'resourceVersion does not match');
      if (o.spec.deletionPolicy === 'Delete') {
        const stored: Integration = { ...o, metadata: { ...o.metadata, deletionTimestamp: new Date().toISOString(), finalizers: ['kudurru.io/integration-cleanup'], resourceVersion: String(++state.revision) } };
        state.objects = state.objects.map((x) => (x === o ? stored : x));
      } else {
        state.objects = state.objects.filter((x) => x !== o);
      }
      return new HttpResponse(null, { status: 204 });
    }),
  ];
}
