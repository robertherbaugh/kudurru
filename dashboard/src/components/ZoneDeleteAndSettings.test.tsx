// Zone settings editing (applyDraft, the section) and the delete dialog,
// against the MSW mock.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ApiClient } from '../api/client';
import { endpoints } from '../api/endpoints';
import type { DNSZoneSpec } from '../api/types';
import { mockDB, signIn } from '../mocks/handlers';
import { ownership } from '../lib/managedBy';
import { API, renderWithApp } from '../test/utils';
import { applyDraft, draftFromZone } from './wizards/ZoneWizard';
import { ZoneDeleteDialog } from './ZoneDeleteDialog';
import { ZoneSettingsSection } from './ZoneSettingsSection';

describe('applyDraft', () => {
  const spec: DNSZoneSpec = {
    zoneName: 'lab.example.',
    type: 'primary',
    nameservers: ['ns1.lab.example'],
    defaultTTL: 600,
    soa: { refresh: 7200, retry: 600 },
    policy: { queryAccess: 'allowOnlyPrivate', transferAccess: 'deny' } as DNSZoneSpec['policy'],
  };

  it('round-trips an unchanged zone', () => {
    expect(applyDraft(spec, draftFromZone(spec))).toEqual(spec);
  });

  it('replaces the shown fields, removes cleared ones and keeps the rest', () => {
    const d = { ...draftFromZone(spec), defaultTTL: '', retry: '', nameservers: ['ns2.lab.example'], queryAccess: 'allow' as const };
    const next = applyDraft(spec, d);
    expect(next.nameservers).toEqual(['ns2.lab.example']);
    expect(next.defaultTTL).toBeUndefined();
    expect(next.soa).toEqual({ refresh: 7200 });
    // queryAccess "allow" is the default and is left out; the transfer policy stays.
    expect(next.policy).toEqual({ transferAccess: 'deny' });
  });

  it('edits a forward zone without touching other forward fields', () => {
    const fwd: DNSZoneSpec = { zoneName: 'corp.example.', type: 'forward', forward: { upstreamGroup: 'corp', dnssecValidation: true } };
    const next = applyDraft(fwd, { ...draftFromZone(fwd), upstreamGroup: 'default', dnssec: '' });
    expect(next.forward).toEqual({ upstreamGroup: 'default' });
  });
});

describe('ZoneDeleteDialog', () => {
  async function seed() {
    signIn('admin');
    const a = endpoints(new ApiClient({ baseUrl: API, csrf: () => 'csrf-mock-token' }));
    await a.objects.create('dnszones', { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: 'lab-example' }, spec: { zoneName: 'lab.example.', type: 'primary' } });
    await a.zones.upsertRecord('lab-example', { dnsName: 'www.lab.example', recordType: 'A', targets: ['192.0.2.1'] });
  }

  it('shows what goes and needs the zone name typed before deleting', async () => {
    await seed();
    const onDeleted = vi.fn();
    renderWithApp(<ZoneDeleteDialog zone="lab-example" zoneName="lab.example." isOpen onClose={() => {}} onDeleted={onDeleted} />);
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText(/1 records in 1 record sets are deleted/)).toBeInTheDocument();
    const del = within(dialog).getByRole('button', { name: 'Delete zone' });
    expect(del).toBeDisabled();
    fireEvent.change(within(dialog).getByRole('textbox', { name: /Type the zone name/ }), { target: { value: 'lab.example' } });
    await waitFor(() => expect(del).toBeEnabled());
    fireEvent.click(del);
    await waitFor(() => expect(onDeleted).toHaveBeenCalled());
    expect(mockDB().zones.some((z) => z.metadata.name === 'lab-example')).toBe(false);
  });

  it('explains a refusal and lists the blocking objects', async () => {
    renderWithApp(<ZoneDeleteDialog zone="home-arpa" zoneName="home.arpa." isOpen onClose={() => {}} />);
    const dialog = await screen.findByRole('dialog');
    expect(await within(dialog).findByText('This zone cannot be deleted here')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Delete zone' })).toBeDisabled();
  });
});

describe('ZoneSettingsSection', () => {
  it('edits a forward zone and saves it', async () => {
    signIn('admin');
    const a = endpoints(new ApiClient({ baseUrl: API, csrf: () => 'csrf-mock-token' }));
    await a.objects.create('dnszones', { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: 'corp-example' }, spec: { zoneName: 'corp.example.', type: 'forward', forward: {} } });
    renderWithApp(<ZoneSettingsSection zone="corp-example" own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    fireEvent.click(within(section).getByRole('button', { name: /Edit/ }));
    expect(await within(section).findByText(/DNSSEC validation/)).toBeInTheDocument();
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    await waitFor(() => expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument());
  });
});
