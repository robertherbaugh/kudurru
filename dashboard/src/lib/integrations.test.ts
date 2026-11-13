import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import type { Integration, IntegrationManifest } from '../api/types';
import {
  buildIntegration,
  commandErrorText,
  configError,
  credentialRefError,
  credentialsError,
  draftOf,
  humanize,
  needsSecrets,
  newDraft,
  providerShape,
  publishes,
  schemaError,
  type JSONSchema,
} from './integrations';
import { ApiError } from '../api/client';

/** The configuration schemas exactly as the Go manifests declare them. */
function goSchema(provider: string): JSONSchema {
  const src = fs.readFileSync(path.resolve(process.cwd(), `../ee/integration/${provider}/manifest.go`), 'utf8');
  const m = /ConfigSchema:\s*json\.RawMessage\(`([^`]+)`\)/.exec(src);
  if (!m?.[1]) throw new Error(`no schema in ${provider}`);
  return JSON.parse(m[1]) as JSONSchema;
}

function manifest(type: string, over: Partial<IntegrationManifest> = {}): IntegrationManifest {
  return { type, displayName: type, version: '1', capabilities: [], ioClass: 'local_polling', configField: type, configSchema: goSchema(type), credentials: [], supported: true, refused: [], ...over };
}

const fieldNames = (m: IntegrationManifest) => (providerShape(m).fields ?? []).map((f) => f.name);

describe('manifest-driven provider shapes', () => {
  it('renders every field of the Go schemas and leaves out what the node refuses', () => {
    expect(fieldNames(manifest('cloudflare'))).toEqual(['zones']);
    const zones = providerShape(manifest('cloudflare')).fields?.[0];
    expect(zones?.type).toBe('list');
    expect(zones?.required).toBe(true);
    expect((zones?.items?.fields ?? []).map((f) => f.name)).toEqual(expect.arrayContaining(['zone', 'zoneID', 'policy', 'adoptUnowned', 'defaults', 'select', 'translate']));
    expect(zones?.items?.fields?.find((f) => f.name === 'policy')?.enum?.map((o) => o.value)).toEqual(['', 'sync', 'upsert-only', 'create-only']);
    expect(zones?.items?.fields?.find((f) => f.name === 'translate')?.type).toBe('map');

    const unifiSQLite = manifest('unifi', { refused: ['spec.unifi.push.dhcpDNSServers', 'spec.unifi.push.mirrorRecords'] });
    expect(fieldNames(unifiSQLite)).toEqual(['endpoint', 'tlsFingerprint', 'site', 'poll', 'legacy', 'inventory', 'groups']);
    const unifiKube = manifest('unifi', { refused: ['spec.unifi.inventory', 'spec.unifi.push.dhcpDNSServers', 'spec.unifi.push.mirrorRecords'] });
    expect(fieldNames(unifiKube)).not.toContain('inventory');
    expect(providerShape(unifiKube).fields?.find((f) => f.name === 'poll')?.type).toBe('duration');
    expect(providerShape(unifiKube).fields?.find((f) => f.name === 'endpoint')?.format).toBe('url');

    expect(fieldNames(manifest('webhook'))).toEqual(['endpoint', 'timeout', 'domains', 'policy']);
  });

  it('validates a block against its schema', () => {
    const unifi = goSchema('unifi');
    expect(schemaError(unifi, {}, '')).toBe('Endpoint is required');
    expect(schemaError(unifi, { endpoint: 'http://unifi.lan' }, '')).toMatch(/Endpoint: An https:\/\/ URL/);
    expect(schemaError(unifi, { endpoint: 'https://unifi.lan', tlsFingerprint: 'sha1:00' }, '')).toMatch(/TLS fingerprint/);
    expect(schemaError(unifi, { endpoint: 'https://unifi.lan', inventory: { zoneRef: { name: 'lan' }, ttl: 0 } }, '')).toMatch(/TTL: at least 1/);
    expect(schemaError(goSchema('cloudflare'), { zones: [] }, '')).toMatch(/at least 1 entry/);
    expect(schemaError(goSchema('cloudflare'), { zones: [{ zone: 'example.org', zoneID: 'XYZ' }] }, '')).toMatch(/Zone ID/);
    expect(schemaError(goSchema('cloudflare'), { zones: [{ zone: 'example.org', policy: 'sync' }] }, '')).toBeNull();
  });

  it('humanizes camelCase names with acronyms', () => {
    expect(humanize('tlsFingerprint')).toBe('TLS fingerprint');
    expect(humanize('zoneID')).toBe('Zone ID');
    expect(humanize('dhcpDNSServers')).toBe('DHCP DNS servers');
    expect(humanize('apiToken')).toBe('API token');
  });
});

describe('credential references', () => {
  it('accepts clean absolute paths and variable names only', () => {
    expect(credentialRefError({ source: 'file', value: '/run/secrets/token' })).toBeNull();
    expect(credentialRefError({ source: 'file', value: 'run/secrets/token' })).toMatch(/absolute/);
    expect(credentialRefError({ source: 'file', value: '/run/../etc/shadow' })).toMatch(/clean/);
    expect(credentialRefError({ source: 'file', value: '/run//x' })).toMatch(/clean/);
    expect(credentialRefError({ source: 'env', value: 'CLOUDFLARE_API_TOKEN' })).toBeNull();
    expect(credentialRefError({ source: 'env', value: '1TOKEN' })).toMatch(/Letters/);
    expect(credentialRefError({ source: '', value: '' })).toBeNull();
  });

  it('requires the required keys in a form this node resolves', () => {
    const m = manifest('cloudflare', { credentials: [{ name: 'apiToken', required: true }] });
    const d = newDraft(m, 'default');
    expect(credentialsError(m, d, ['file'])).toBe('API token is required');
    d.credentials.apiToken = { source: 'env', value: 'TOKEN' };
    expect(credentialsError(m, d, ['file'])).toMatch(/does not resolve env/);
    expect(credentialsError(m, d, ['file', 'env'])).toBeNull();
  });
});

describe('documents', () => {
  const m = manifest('cloudflare', { capabilities: ['RecordSink'], credentials: [{ name: 'apiToken', required: true }] });

  it('builds a document with references only and keeps fields the form does not show', () => {
    const d = newDraft(m, 'default');
    d.name = 'public';
    d.config = { zones: [{ zone: 'example.org', policy: 'sync', adoptUnowned: false, defaults: {}, select: {}, translate: { __rows: [] } }] };
    d.credentials.apiToken = { source: 'file', value: '/run/secrets/cf' };
    d.deletionPolicy = 'Delete';
    expect(configError(m, d)).toBeNull();
    const doc = buildIntegration(m, d);
    expect(doc).toEqual({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'public', namespace: 'default' },
      spec: { type: 'cloudflare', cloudflare: { zones: [{ zone: 'example.org', policy: 'sync' }] }, credentials: { apiToken: { file: '/run/secrets/cf' } }, deletionPolicy: 'Delete' },
    });
    expect(JSON.stringify(doc)).not.toMatch(/"value"/);

    const stored: Integration = { ...doc, metadata: { ...doc.metadata, uid: 'u', resourceVersion: '7' }, spec: { ...doc.spec, cloudflare: { zones: [{ zone: 'example.org', future: 1 }] } } };
    const again = buildIntegration(m, draftOf(stored, m), stored);
    expect(again.metadata.resourceVersion).toBe('7');
    expect(again.spec.cloudflare).toEqual({ zones: [{ zone: 'example.org', future: 1 }] });
  });

  it('decides whether a sync publishes from the capability and the destinations', () => {
    expect(publishes(m, { type: 'cloudflare', cloudflare: { zones: [{ zone: 'a' }] } })).toBe(true);
    const webhook = manifest('webhook', { capabilities: ['Actions', 'ClientSource', 'Hook', 'RecordSink'] });
    expect(publishes(webhook, { type: 'webhook', webhook: { endpoint: 'http://127.0.0.1:1' } })).toBe(false);
    expect(publishes(webhook, { type: 'webhook', webhook: { endpoint: 'http://127.0.0.1:1', domains: ['example.org'] } })).toBe(true);
    expect(publishes(manifest('unifi', { capabilities: ['ClientSource'] }), { type: 'unifi', unifi: { endpoint: 'https://u' } })).toBe(false);
  });

  it('needs integrations.secrets to bind a credential or move where a bound one is sent', () => {
    const base = { type: 'unifi', unifi: { endpoint: 'https://a' }, credentials: { apiKey: { file: '/k' } } };
    expect(needsSecrets(base, { ...base, suspend: true })).toBe(false);
    expect(needsSecrets(base, { ...base, unifi: { endpoint: 'https://b' } })).toBe(true);
    expect(needsSecrets(base, { ...base, credentials: { apiKey: { env: 'K' } } })).toBe(true);
    expect(needsSecrets(undefined, { type: 'webhook', webhook: { endpoint: 'x' } })).toBe(false);
    expect(needsSecrets({ type: 'webhook', webhook: { endpoint: 'x' } }, { type: 'webhook', webhook: { endpoint: 'y' } })).toBe(false);
  });

  it('names a stale preview, a stale release and an uncertain outcome for what they are', () => {
    const conflict = new ApiError(409, 'conflict', 'original integration review or authority changed');
    expect(commandErrorText(conflict, 'sync')).toMatch(/preview expired or no longer matches/);
    expect(commandErrorText(conflict, 'release')).toMatch(/not terminating any more/);
    expect(commandErrorText(new ApiError(503, 'integration_uncertain', 'x'), 'sync')).toMatch(/not sent again/);
    expect(commandErrorText(new ApiError(500, 'internal', 'x'), 'sync')).toBeNull();
  });
});
