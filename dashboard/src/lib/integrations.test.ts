import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import type { Integration, IntegrationFieldLimit, IntegrationManifest } from '../api/types';
import {
  buildIntegration,
  commandErrorText,
  configError,
  configValue,
  credentialRefError,
  credentialsError,
  DESTINATION_FIELDS,
  draftOf,
  FIELD_CONDITIONS,
  humanize,
  needsSecrets,
  newDraft,
  notOfferedFields,
  providerShape,
  PUBLISH_RULES,
  publishes,
  schemaError,
  type JSONSchema,
} from './integrations';
import { ApiError } from '../api/client';
import { formatError } from '../components/fields/formats';
import { mockManifests } from '../mocks/integrations';
import { emptyDraft } from './shapes';
import type { UiShape } from './settingsMeta';

/** The configuration schemas exactly as the Go manifests declare them. */
function goSchema(provider: string): JSONSchema {
  const src = fs.readFileSync(path.resolve(process.cwd(), `../ee/integration/${provider}/manifest.go`), 'utf8');
  const m = /ConfigSchema:\s*json\.RawMessage\(`([^`]+)`\)/.exec(src);
  if (!m?.[1]) throw new Error(`no schema in ${provider}`);
  return JSON.parse(m[1]) as JSONSchema;
}

function manifest(type: string, over: Partial<IntegrationManifest> = {}): IntegrationManifest {
  return { type, displayName: type, version: '1', capabilities: [], ioClass: 'local_polling', configField: type, configSchema: goSchema(type), credentials: [], supported: true, refused: [], fieldLimits: [], ...over };
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

    // SQLite runs every UniFi write (D401): the push block is offered whole.
    const limitsOf = (driver: 'sqlite' | 'kube') => {
      const m = mockManifests(driver).find((x) => x.type === 'unifi');
      return { refused: m?.refused ?? ['?'], fieldLimits: m?.fieldLimits ?? [] };
    };
    const unifiSQLite = manifest('unifi', limitsOf('sqlite'));
    expect(notOfferedFields(unifiSQLite)).toEqual([]);
    expect(fieldNames(unifiSQLite)).toEqual(['endpoint', 'tlsFingerprint', 'site', 'poll', 'legacy', 'inventory', 'groups', 'push']);
    const push = providerShape(unifiSQLite).fields?.find((f) => f.name === 'push');
    expect(push?.type).toBe('object');
    expect(push?.required).toBe(false);
    const pushField = (name: string) => push?.fields?.find((f) => f.name === name);
    expect((push?.fields ?? []).map((f) => [f.name, f.type])).toEqual([['dhcpDNSServers', 'bool'], ['dnsServers', 'string[]'], ['networks', 'string[]'], ['clientActions', 'bool'], ['mirrorRecords', 'object']]);
    expect(pushField('dnsServers')?.items?.format).toBe('ipv4');
    expect(pushField('dnsServers')?.label).toBe('DNS servers');
    // The schema's description is the field's help.
    expect(pushField('dnsServers')?.help).toMatch(/canonical dotted form.*0\.0\.0\.0, loopback, multicast and 255\.255\.255\.255/);
    const mirror = pushField('mirrorRecords');
    expect((mirror?.fields ?? []).map((f) => [f.name, f.type])).toEqual([['enabled', 'bool'], ['types', 'string[]'], ['domains', 'string[]'], ['labels', 'map'], ['exclude', 'string[]']]);
    expect(mirror?.fields?.find((f) => f.name === 'types')?.items?.enum?.map((o) => o.value)).toEqual(['A', 'CNAME']);
    // Kubernetes refuses inventory publication and the three UniFi write
    // switches; the manifest names the settings only those switches use as
    // inert and requiring them, so no push field is left.
    const unifiKube = manifest('unifi', limitsOf('kube'));
    expect(fieldNames(unifiKube)).toEqual(['endpoint', 'tlsFingerprint', 'site', 'poll', 'legacy', 'groups']);
    expect(notOfferedFields(unifiKube)).toEqual([
      { effect: 'refused', reason: 'RecordPublicationUnsupportedOnKubernetes', message: expect.stringMatching(/^UniFi DNS record publication \(spec\.unifi\.inventory\)/), fields: ['spec.unifi.inventory'] },
      { effect: 'refused', reason: 'UniFiPushUnsupportedOnKubernetes', message: expect.stringMatching(/run only on the SQLite store/), fields: ['spec.unifi.push.dhcpDNSServers', 'spec.unifi.push.mirrorRecords.enabled', 'spec.unifi.push.clientActions'] },
      { effect: 'inert', reason: '', message: expect.stringMatching(/^Used only by spec\.unifi\.push\.dhcpDNSServers/), fields: ['spec.unifi.push.dnsServers', 'spec.unifi.push.networks'] },
      { effect: 'inert', reason: '', message: expect.stringMatching(/^Used only by spec\.unifi\.push\.mirrorRecords\.enabled/), fields: ['spec.unifi.push.mirrorRecords.types', 'spec.unifi.push.mirrorRecords.domains', 'spec.unifi.push.mirrorRecords.labels', 'spec.unifi.push.mirrorRecords.exclude'] },
    ]);
    expect(providerShape(unifiKube).fields?.find((f) => f.name === 'poll')?.type).toBe('duration');
    expect(providerShape(unifiKube).fields?.find((f) => f.name === 'endpoint')?.format).toBe('url');

    expect(fieldNames(manifest('webhook'))).toEqual(['endpoint', 'timeout', 'domains', 'policy']);
  });

  it('offers or hides each field as the manifest\'s field limits say', () => {
    const push = (m: IntegrationManifest) => providerShape(m).fields?.find((f) => f.name === 'push');
    // Only the mirror switch is refused: its settings require it and go too,
    // the DHCP and client switches and their settings stay.
    const mirrorOff = manifest('unifi', {
      refused: ['spec.unifi.push.mirrorRecords.enabled'],
      fieldLimits: [
        { field: 'spec.unifi.push.mirrorRecords.enabled', effect: 'refused', reason: 'R', message: 'Refused here.' },
        { field: 'spec.unifi.push.mirrorRecords.domains', effect: 'inert', requires: 'spec.unifi.push.mirrorRecords.enabled', message: 'Needs the mirror.' },
        // An inert field whose required field is offered is offered, with its message.
        { field: 'spec.unifi.push.networks', effect: 'inert', requires: 'spec.unifi.push.dhcpDNSServers', message: 'Only with DHCP DNS servers.' },
      ],
    });
    const p = push(mirrorOff);
    expect((p?.fields ?? []).map((f) => f.name)).toEqual(['dhcpDNSServers', 'dnsServers', 'networks', 'clientActions', 'mirrorRecords']);
    expect((p?.fields?.find((f) => f.name === 'mirrorRecords')?.fields ?? []).map((f) => f.name)).toEqual(['types', 'labels', 'exclude']);
    expect(p?.fields?.find((f) => f.name === 'networks')?.help).toBe('Only with DHCP DNS servers.');
    expect(notOfferedFields(mirrorOff)).toEqual([
      { effect: 'refused', reason: 'R', message: 'Refused here.', fields: ['spec.unifi.push.mirrorRecords.enabled'] },
      { effect: 'inert', reason: '', message: 'Needs the mirror.', fields: ['spec.unifi.push.mirrorRecords.domains'] },
    ]);
    // An inert field that requires an inert field left out is left out too.
    const chained = manifest('unifi', {
      refused: ['spec.unifi.push.dhcpDNSServers'],
      fieldLimits: [
        { field: 'spec.unifi.push.dhcpDNSServers', effect: 'refused', reason: 'R', message: 'Refused here.' },
        { field: 'spec.unifi.push.networks', effect: 'inert', requires: 'spec.unifi.push.dnsServers', message: 'Needs the servers.' },
        { field: 'spec.unifi.push.dnsServers', effect: 'inert', requires: 'spec.unifi.push.dhcpDNSServers', message: 'Needs DHCP.' },
      ],
    });
    expect((push(chained)?.fields ?? []).map((f) => f.name)).toEqual(['clientActions', 'mirrorRecords']);
    // A refused path without a limit (an older server) is still left out.
    const bare = manifest('unifi', { refused: ['spec.unifi.inventory'] });
    expect(fieldNames(bare)).not.toContain('inventory');
    expect(notOfferedFields(bare)).toEqual([{ effect: 'refused', reason: '', message: 'This node refuses an instance that sets it.', fields: ['spec.unifi.inventory'] }]);
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
    // The push block: IPv4 strings (format ipv4), bounded unique lists.
    const base = { endpoint: 'https://unifi.lan' };
    expect(schemaError(unifi, { ...base, push: { dnsServers: ['192.0.2.53', '192.0.2.54'] } }, '')).toBeNull();
    expect(schemaError(unifi, { ...base, push: { dnsServers: ['2001:db8::53'] } }, '')).toBe('Push DNS servers #1: Not an IPv4 address');
    expect(schemaError(unifi, { ...base, push: { dnsServers: ['192.0.2.053'] } }, '')).toBe('Push DNS servers #1: Not an IPv4 address');
    expect(schemaError(unifi, { ...base, push: { dnsServers: ['192.0.2.1', '192.0.2.2', '192.0.2.3', '192.0.2.4', '192.0.2.5'] } }, '')).toMatch(/at most 4 entries/);
    expect(schemaError(unifi, { ...base, push: { networks: ['LAN', 'LAN'] } }, '')).toMatch(/Push Networks: lists an entry twice/);
    expect(schemaError(unifi, { ...base, push: { mirrorRecords: { types: ['AAAA'] } } }, '')).toMatch(/one of A, CNAME/);
    expect(schemaError(unifi, { ...base, push: { mirrorRecords: { enabled: true, domains: ['example.com'], labels: { mirror: 'unifi' } } } }, '')).toBeNull();
  });

  it('refuses exactly the DNS server forms the Go schema pattern refuses', () => {
    const items = goSchema('unifi').properties?.push?.properties?.dnsServers?.items ?? {};
    expect(items.format).toBe('ipv4');
    const pattern = new RegExp(items.pattern ?? '(?!)');
    // The forms ee/integration/unifi TestManifestDNSServerPatternMatchesValidation tries, and the addresses it refuses beyond the form.
    const forms = ['192.0.2.53', '10.0.0.1', '255.255.255.254', '1.2.3.4', '192.0.2.053', '01.2.3.4', '1.2.3', '1.2.3.4.5', '256.1.1.1', '::1', ' 1.2.3.4', '1.2.3.4\n', '1.02.3.4', '0.0.0.0', '127.0.0.1', '224.0.0.1', '255.255.255.255'];
    for (const v of forms) {
      // The check of the built block agrees with the pattern exactly.
      expect(schemaError(items, v, 'x') === null, v).toBe(pattern.test(v));
      // A list row is trimmed before it is sent (lib/shapes fromDraft), so
      // its inline check (format ipv4) agrees with the pattern on what is sent.
      expect(formatError('ipv4', v) === null, v).toBe(pattern.test(v.trim()));
    }
  });

  it('checks the conditions between push fields that the provider checks', () => {
    const unifi = manifest('unifi');
    const draft = (config: Record<string, unknown>) => ({ ...newDraft(unifi, 'default'), config: draftOf({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'g' }, spec: { type: 'unifi', unifi: config } }, unifi).config });
    const base = { endpoint: 'https://unifi.lan' };
    expect(configError(unifi, draft({ ...base, push: { dhcpDNSServers: true } }))).toMatch(/at least one DNS server and one network/);
    expect(configError(unifi, draft({ ...base, push: { dhcpDNSServers: true, dnsServers: ['192.0.2.53'] } }))).toMatch(/one network/);
    expect(configError(unifi, draft({ ...base, push: { dhcpDNSServers: true, dnsServers: ['192.0.2.53'], networks: ['LAN'] } }))).toBeNull();
    // The parameters alone are inert, as on the server.
    expect(configError(unifi, draft({ ...base, push: { dnsServers: ['192.0.2.53'] } }))).toBeNull();
    expect(configError(unifi, draft({ ...base, push: { clientActions: true } }))).toMatch(/needs Legacy/);
    expect(configError(unifi, draft({ ...base, legacy: true, push: { clientActions: true } }))).toBeNull();
    expect(configError(unifi, draft({ ...base, push: { mirrorRecords: { enabled: true } } }))).toMatch(/at least one domain/);
    expect(configError(unifi, draft({ ...base, push: { mirrorRecords: { domains: ['example.com'] } } }))).toBeNull();
    const full = { ...base, legacy: true, push: { dhcpDNSServers: true, dnsServers: ['192.0.2.53'], networks: ['LAN', 'IoT'], clientActions: true, mirrorRecords: { enabled: true, types: ['A'], domains: ['example.com'], exclude: ['*.internal.example.com'], labels: { mirror: 'unifi' } } } };
    const d = draft(full);
    expect(configError(unifi, d)).toBeNull();
    expect(configValue(unifi, d)).toEqual(full);

    // ee/integration/unifi validatePush checks exactly these conditions.
    const src = fs.readFileSync(path.resolve(process.cwd(), '../ee/integration/unifi/push.go'), 'utf8');
    const body = /func validatePush\(c public\.UniFiIntegration\) error \{([\s\S]*?)\n\}/.exec(src)?.[1] ?? '';
    expect(body).toContain('if push.DHCPDNSServers {\n\t\tif len(push.DNSServers) == 0 || len(push.DNSServers) > 4 || len(push.Networks) == 0');
    expect(body).toContain('if push.ClientActions && !c.Legacy {');
    expect(body).toContain('if m.Enabled && len(m.Domains) == 0 ||');
    expect(FIELD_CONDITIONS.map((c) => [c.when, c.needs])).toEqual([
      ['spec.unifi.push.dhcpDNSServers', ['spec.unifi.push.dnsServers', 'spec.unifi.push.networks']],
      ['spec.unifi.push.clientActions', ['spec.unifi.legacy']],
      ['spec.unifi.push.mirrorRecords.enabled', ['spec.unifi.push.mirrorRecords.domains']],
    ]);
  });

  it('creates and saves minimal instances against the real schemas (optional objects stay optional)', () => {
    // The wizard path: a new draft, then a fresh zone row as the list editor adds it.
    const cf = manifest('cloudflare', { capabilities: ['RecordSink'] });
    const zones = providerShape(cf).fields?.find((f) => f.name === 'zones');
    const d = newDraft(cf, 'default');
    d.config = { zones: [{ ...(emptyDraft(zones?.items as UiShape) as Record<string, unknown>), zone: 'example.org' }] };
    expect(configError(cf, d)).toBeNull();
    expect(configValue(cf, d)).toEqual({ zones: [{ zone: 'example.org' }] });
    // Format and bounds checks of the fields inside an optional object still apply.
    const zone = (d.config as { zones: Record<string, unknown>[] }).zones[0] as Record<string, unknown>;
    zone.adoptFrom = { externalDNSOwner: 'edns-a' };
    expect(configError(cf, d)).toBeNull();
    expect(configValue(cf, d)).toEqual({ zones: [{ zone: 'example.org', adoptFrom: { externalDNSOwner: 'edns-a' } }] });
    zone.defaults = { proxied: false, ttl: '0', comment: '' };
    expect(configError(cf, d)).toMatch(/TTL/);

    // A stored zone opens and saves unchanged.
    const stored: Integration = { apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'cf', namespace: 'default', uid: 'u', resourceVersion: '1' }, spec: { type: 'cloudflare', cloudflare: { zones: [{ zone: 'example.com' }] } } };
    const sd = draftOf(stored, cf);
    expect(configError(cf, sd)).toBeNull();
    sd.suspend = true;
    expect(configError(cf, sd)).toBeNull();
    expect(buildIntegration(cf, sd, stored).spec.cloudflare).toEqual({ zones: [{ zone: 'example.com' }] });

    // UniFi on SQLite without an inventory zone, new and stored.
    const unifi = manifest('unifi');
    const ud = newDraft(unifi, 'default');
    expect(configError(unifi, ud)).toBe('Endpoint is required');
    (ud.config as Record<string, unknown>).endpoint = 'https://unifi.example';
    expect(configError(unifi, ud)).toBeNull();
    expect(configValue(unifi, ud)).toEqual({ endpoint: 'https://unifi.example' });
    const us = draftOf({ ...stored, spec: { type: 'unifi', unifi: { endpoint: 'https://unifi.example' } } }, unifi);
    expect(configError(unifi, us)).toBeNull();
    // Setting part of the inventory makes its zone required, as on the server.
    ((us.config as Record<string, unknown>).inventory as Record<string, unknown>).ttl = '60';
    expect(configError(unifi, us)).toBe('Inventory Zone ref is required');
    ((us.config as Record<string, unknown>).inventory as Record<string, unknown>).zoneRef = { name: 'lan', namespace: '' };
    expect(configError(unifi, us)).toBeNull();
  });

  it('serves the exact Go schemas from the dashboard mock', () => {
    for (const m of mockManifests('sqlite')) expect(m.configSchema).toEqual(goSchema(m.type));
  });

  it('serves the Go capabilities and each driver\'s refusals from the dashboard mock', () => {
    const goSource = (rel: string) => fs.readFileSync(path.resolve(process.cwd(), `../${rel}`), 'utf8');
    for (const m of mockManifests('sqlite')) {
      const caps = /Capabilities: \[\]integration\.Capability\{([^}]*)\}/.exec(goSource(`ee/integration/${m.type}/manifest.go`))?.[1] ?? '';
      expect(m.capabilities, m.type).toEqual([...caps.matchAll(/integration\.Cap(\w+)/g)].map((x) => x[1]));
    }
    // cmd/kudurru integration_manifests.go, per driver: the field limits as
    // the Go code writes them, with each reason constant and message resolved.
    const src = goSource('cmd/kudurru/integration_manifests.go');
    const kubeSrc = goSource('cmd/kudurru/integration_kube.go');
    expect(src).toMatch(/sqliteIntegrationLimits = integrationDriverLimits\{\}/);
    const kube = /kubeIntegrationLimits\s*=\s*integrationDriverLimits\{([\s\S]*?)\n\t\}\n\)/.exec(src)?.[1] ?? '';
    const constant = (name: string) => {
      const v = new RegExp(`\\b${name}\\s*=\\s*("(?:[^"\\\\]|\\\\.)*")`).exec(src + kubeSrc)?.[1];
      if (!v) throw new Error(`no constant ${name}`);
      return JSON.parse(v) as string;
    };
    const refusalMessage = (reason: string) => {
      const v = new RegExp(`case ${reason}:\\n\\t\\treturn ("(?:[^"\\\\]|\\\\.)*")`).exec(kubeSrc)?.[1];
      if (!v) throw new Error(`no message for ${reason}`);
      return JSON.parse(v) as string;
    };
    const message = (expr: string) => {
      const call = /^kubeRefusalMessage\((\w+), ""\)$/.exec(expr);
      if (call) return refusalMessage(call[1] ?? '');
      return expr.startsWith('"') ? (JSON.parse(expr) as string) : constant(expr);
    };
    const fields = /fields: map\[string\]\[\]api\.IntegrationFieldLimit\{"(\w+)": \{([\s\S]*?)\n\t\t\}\}/.exec(kube);
    expect(fields?.[1]).toBe('unifi');
    const goLimits = [...(fields?.[2] ?? '').matchAll(/^\t+\{(.*)\},$/gm)].map((line): IntegrationFieldLimit => {
      const body = line[1] ?? '';
      const str = (k: string) => new RegExp(`${k}: "([^"]*)"`).exec(body)?.[1];
      const reason = /Reason: (\w+)/.exec(body)?.[1];
      const requires = str('Requires');
      const effect = str('Effect');
      expect(effect === 'refused' || effect === 'inert', body).toBe(true);
      return { field: str('Field') ?? '', effect: effect === 'inert' ? 'inert' : 'refused', ...(reason ? { reason: constant(reason) } : {}), ...(requires ? { requires } : {}), message: message(/Message: (.*)$/.exec(body)?.[1] ?? '') };
    });
    expect(goLimits).toHaveLength(10);
    const unsupported: Record<string, string> = Object.fromEntries([...(/unsupported: map\[string\]string\{(.*)\}/.exec(kube)?.[1] ?? '').matchAll(/"(\w+)": (\w+)/g)].map((x) => [x[1] ?? '', constant(x[2] ?? '')]));
    expect(unsupported).toEqual({ webhook: 'UnsupportedOnKubernetes' });
    for (const m of mockManifests('kube')) {
      const limits = m.type === 'unifi' ? goLimits : [];
      expect(m.fieldLimits, m.type).toEqual(limits);
      // refused keeps its shape: the fields whose limit refuses.
      expect(m.refused, m.type).toEqual(limits.filter((l) => l.effect === 'refused').map((l) => l.field));
      expect(m.supported, m.type).toBe(!unsupported[m.type]);
      expect(m.reason, m.type).toBe(unsupported[m.type]);
    }
    for (const m of mockManifests('sqlite')) {
      expect(m.refused, m.type).toEqual([]);
      expect(m.fieldLimits, m.type).toEqual([]);
      expect(m.supported, m.type).toBe(true);
    }
    // The refused switches are the ones the host refuses for those reasons.
    expect(kubeSrc).toMatch(/d\.Spec\.UniFi\.Inventory != nil:\n\t\treturn reasonRecordsUnsupported/);
    expect(kubeSrc).toMatch(/\(d\.Spec\.UniFi\.Push\.DHCPDNSServers \|\| d\.Spec\.UniFi\.Push\.MirrorRecords\.Enabled \|\| d\.Spec\.UniFi\.Push\.ClientActions\):[^\n]*\n(?:\t\t\/\/[^\n]*\n)*\t\treturn reasonPushUnsupported/);
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
    // UniFi mirroring records as DNS policies is a reviewed-only publisher (D401).
    const unifi = manifest('unifi', { capabilities: ['ClientSource', 'RecordSink', 'Actions'] });
    expect(publishes(unifi, { type: 'unifi', unifi: { endpoint: 'https://u', push: { dhcpDNSServers: true, clientActions: true } } })).toBe(false);
    expect(publishes(unifi, { type: 'unifi', unifi: { endpoint: 'https://u', push: { mirrorRecords: { domains: ['example.com'] } } } })).toBe(false);
    expect(publishes(unifi, { type: 'unifi', unifi: { endpoint: 'https://u', push: { mirrorRecords: { enabled: true, domains: ['example.com'] } } } })).toBe(true);
  });

  it('keeps the publish rule and the destination fields in step with the server', () => {
    const goSource = (rel: string) => fs.readFileSync(path.resolve(process.cwd(), `../${rel}`), 'utf8');
    // internal/api integrationDestinationFields, as dotted paths.
    const fields = /var integrationDestinationFields = \[\]\[\]string\{([\s\S]*?)\n\}/.exec(goSource('internal/api/integration_objects.go'))?.[1] ?? '';
    const server = [...fields.matchAll(/\{([^}]*)\}/g)].map((m) => [...(m[1] ?? '').matchAll(/"([^"]+)"/g)].map((x) => x[1]).join('.'));
    expect(server.length).toBeGreaterThan(0);
    expect([...DESTINATION_FIELDS].sort()).toEqual([...server].sort());

    // cmd/kudurru integrationPublishes: "a Cloudflare block, a webhook with
    // domains, or a UniFi block with the record mirror enabled" (D380,
    // D401). publishes() states it per block in PUBLISH_RULES. When this
    // fails, change PUBLISH_RULES (or have the manifest report it)
    // together with the server rule.
    const body = /func integrationPublishes\(d \*public\.Integration\) bool \{\s*([^\n]+)\n\}/.exec(goSource('cmd/kudurru/integration_runtime.go'))?.[1]?.trim();
    expect(body).toBe('return d.Spec.Cloudflare != nil || d.Spec.Webhook != nil && len(d.Spec.Webhook.Domains) > 0 || d.Spec.UniFi != nil && d.Spec.UniFi.Push.MirrorRecords.Enabled');
    expect(Object.keys(PUBLISH_RULES).sort()).toEqual([...new Set([...(body ?? '').matchAll(/d\.Spec\.(\w+) != nil/g)].map((x) => (x[1] ?? '').toLowerCase()))].sort());
    const lists = (type: string) => Object.entries(goSchema(type).properties ?? {}).filter(([, v]) => v.type === 'array').map(([k]) => k);
    expect(lists('cloudflare')).toEqual(['zones']);
    expect(goSchema('cloudflare').required).toContain('zones');
    expect(goSchema('cloudflare').properties?.zones?.minItems).toBe(1);
    expect(lists('webhook')).toEqual(['domains']);
    for (const m of mockManifests('sqlite')) {
      const src = goSource(`ee/integration/${m.type}/manifest.go`);
      expect(m.capabilities.includes('RecordSink'), m.type).toBe(/integration\.CapRecordSink/.test(src));
      // Every record sink has a publish rule, and only record sinks.
      expect(m.type in PUBLISH_RULES, m.type).toBe(m.capabilities.includes('RecordSink'));
    }
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
