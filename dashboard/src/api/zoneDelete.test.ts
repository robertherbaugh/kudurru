// DELETE /zones/{zone} (D148) through the endpoint and the MSW mock.
import { describe, expect, it } from 'vitest';
import { mockDB, signIn } from '../mocks/handlers';
import { API } from '../test/utils';
import { ApiClient, ApiError } from './client';
import { endpoints } from './endpoints';
import type { ApiErrorBody } from './types';

function api() {
  signIn('admin');
  return endpoints(new ApiClient({ baseUrl: API, csrf: () => 'csrf-mock-token' }));
}

describe('zones.delete', () => {
  it('previews with dryRun, then deletes the zone with its record sets', async () => {
    const a = api();
    await a.objects.create('dnszones', {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'DNSZone',
      metadata: { name: 'lab-example' },
      spec: { zoneName: 'lab.example.', type: 'primary' },
    });
    await a.zones.upsertRecord('lab-example', { dnsName: 'www.lab.example', recordType: 'A', targets: ['192.0.2.1'] });
    await a.zones.upsertRecord('lab-example', { dnsName: 'db.lab.example', recordType: 'A', targets: ['192.0.2.2'] });

    const dry = await a.zones.delete('lab-example', { dryRun: true });
    expect(dry).toMatchObject({ zone: 'lab-example', dryRun: true, recordSetCount: 1, endpointCount: 2, recordSets: ['default/api-lab-example'] });
    expect(mockDB().zones.some((z) => z.metadata.name === 'lab-example')).toBe(true);

    const res = await a.zones.delete('lab-example');
    expect(res).toMatchObject({ dryRun: false, recordSetCount: 1, endpointCount: 2 });
    expect(mockDB().zones.some((z) => z.metadata.name === 'lab-example')).toBe(false);
    expect(mockDB().recordSets.some((rs) => rs.spec.zoneRef.name === 'lab-example')).toBe(false);
  });

  it('refuses the root zone and zones with managed record sets', async () => {
    const a = api();
    const zones = await a.zones.list();
    const refusal = Object.fromEntries(zones.items.map((z) => [z.name, z.deleteRefusal]));
    expect(refusal).toMatchObject({ root: 'default_zone', 'home-arpa': 'managed_by', 'example-internal': 'managed_by' });

    const root = await a.zones.delete('root', { dryRun: true }).catch((e: unknown) => e);
    expect(root).toBeInstanceOf(ApiError);
    expect((root as ApiError).key).toBe('default_zone');

    const managed = await a.zones.delete('example-internal').catch((e: unknown) => e);
    expect((managed as ApiError).status).toBe(409);
    expect((managed as ApiError).owner).toBe('integration:external-dns');
    expect(mockDB().zones.some((z) => z.metadata.name === 'example-internal')).toBe(true);
  });

  it('the object route refuses a zone that record sets reference', async () => {
    signIn('admin');
    const res = await fetch(`${API}/objects/dnszones/example-internal`, { method: 'DELETE', headers: { 'X-Kudurru-CSRF': 'csrf-mock-token' } });
    expect(res.status).toBe(409);
    const body = (await res.json()) as ApiErrorBody;
    expect(body.error.key).toBe('zone_not_empty');
    expect(body.error.objects?.length).toBe(2);
  });
});
