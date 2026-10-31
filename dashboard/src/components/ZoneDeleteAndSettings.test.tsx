// Zone settings editing (applyDraft, the section) and the delete dialog,
// against the MSW mock.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { ApiClient } from '../api/client';
import { endpoints } from '../api/endpoints';
import type { DNSZoneSpec } from '../api/types';
import { mockDB, signIn } from '../mocks/handlers';
import { ownership } from '../lib/managedBy';
import { API, renderWithApp } from '../test/utils';
import { findSelector, pick } from '../test/controls';
import { server } from '../test/setup';
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

  it('round-trips and disables a primary zone fallback without changing other settings', () => {
    const overlay: DNSZoneSpec = { ...spec, fallback: { upstreamGroup: 'resolver' } };
    expect(applyDraft(overlay, draftFromZone(overlay))).toEqual(overlay);
    const d = draftFromZone(overlay);
    expect(applyDraft(overlay, { ...d, fallbackGroup: '' }).fallback).toEqual({});
    expect(applyDraft(overlay, { ...d, fallback: false })).toEqual(spec);
  });

  it('preserves secondary protocol, policy, SOA and other settings while clearing the optional key', () => {
    const secondary: DNSZoneSpec = {
      zoneName: 'secondary.example.', type: 'secondary',
      secondary: { primaries: ['ns1.example.com'], tsigKeyRef: 'old-key.', transferProtocol: 'tls' },
      policy: { queryAccess: 'allowOnlyPrivate', notify: 'none', zoneTransfer: 'deny' },
      soa: { refresh: 7200 }, defaultTTL: 900, recordSetSelector: { namespaces: ['other'] },
    };
    const draft = draftFromZone(secondary);
    expect(draft.type).toBe('secondary');
    expect(applyDraft(secondary, draft)).toEqual(secondary);
    expect(applyDraft(secondary, { ...draft, apex: 'wrong.example', primaries: [' ns2.example.com:5353 ', ''], tsigKeyRef: '' })).toEqual({
      ...secondary, secondary: { primaries: ['ns2.example.com:5353'], transferProtocol: 'tls' },
    });
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
  it('enables missing-name resolution and selects native recursion', async () => {
    signIn('admin');
    const a = endpoints(new ApiClient({ baseUrl: API, csrf: () => 'csrf-mock-token' }));
    await a.objects.create('dnszones', { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: 'public-example' }, spec: { zoneName: 'public.example.', type: 'primary' } });
    renderWithApp(<ZoneSettingsSection zone="public-example" own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    fireEvent.click(within(section).getByRole('button', { name: /Edit/ }));
    const enabled = await within(section).findByRole('checkbox', { name: 'Resolve missing names publicly' });
    expect(enabled).not.toBeChecked();
    fireEvent.click(enabled);
    const selector = await findSelector('Fallback upstream group');
    pick(selector, /^resolver$/);
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    await waitFor(() => expect(mockDB().zones.find((z) => z.metadata.name === 'public-example')?.spec.fallback).toEqual({ upstreamGroup: 'resolver' }));
    expect(await within(section).findByText('Resolve through resolver')).toBeInTheDocument();
  });

  it('edits stub bootstrap servers with If-Match and preserves access policy', async () => {
    const z = {
      apiVersion: 'kudurru.io/v1alpha1' as const, kind: 'DNSZone' as const,
      metadata: { name: 'stub.example', resourceVersion: '15', labels: { owner: 'dns-team' } },
      spec: { zoneName: 'stub.example.', type: 'stub' as const, stub: { nameservers: ['192.0.2.53'] }, policy: { queryAccess: 'allowOnlyPrivate' as const } },
    };
    mockDB().zones.push(z);
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    await waitFor(() => expect(edit).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(edit);
    fireEvent.change(within(section).getByRole('textbox', { name: 'Bootstrap servers 1' }), { target: { value: 'tcp://ns.example:5353' } });
    const saveRequest = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, async ({ request }) => {
      const body = await request.json();
      saveRequest(request.headers.get('If-Match'), body);
      return HttpResponse.json(body);
    }));
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    await waitFor(() => expect(saveRequest).toHaveBeenCalledWith('15', { ...z, spec: { ...z.spec, stub: { nameservers: ['tcp://ns.example:5353'] } } }));
  });

  function seedSecondary(protocol: 'tcp' | 'tls' = 'tcp') {
    const z = {
      apiVersion: 'kudurru.io/v1alpha1' as const, kind: 'DNSZone' as const,
      metadata: { name: 'secondary.example', resourceVersion: '12', labels: { owner: 'dns-team' } },
      status: { serial: 123 },
      spec: {
        zoneName: 'secondary.example.', type: 'secondary' as const,
        secondary: { primaries: ['ns1.example.com'], tsigKeyRef: 'old-key.', transferProtocol: protocol },
        policy: { queryAccess: 'allowOnlyPrivate' as const, zoneTransfer: 'deny' as const },
      },
    };
    mockDB().zones.push(z);
    return z;
  }

  it.each(['tcp', 'tls'] as const)('saves secondary settings from %s with If-Match while preserving policy and metadata', async (protocol) => {
    const z = seedSecondary(protocol);
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    await waitFor(() => expect(edit).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(edit);
    expect(within(section).queryByRole('combobox', { name: 'Refresh' })).not.toBeInTheDocument();
    expect(within(section).getByRole('radio', { name: protocol.toUpperCase() })).toBeChecked();
    fireEvent.click(within(section).getByRole('radio', { name: 'TLS' }));
    fireEvent.change(within(section).getByRole('textbox', { name: 'Primary servers 1' }), { target: { value: 'ns2.example.com:5353' } });
    fireEvent.change(within(section).getByRole('textbox', { name: /^TSIG key name/ }), { target: { value: '' } });
    const saveRequest = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, async ({ request }) => {
      const body = await request.json();
      saveRequest(request.headers.get('If-Match'), body);
      return HttpResponse.json(body);
    }));
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    await waitFor(() => expect(saveRequest).toHaveBeenCalledWith('12', {
      apiVersion: z.apiVersion, kind: z.kind, metadata: z.metadata,
      spec: { ...z.spec, secondary: { primaries: ['ns2.example.com:5353'], transferProtocol: 'tls' } },
    }));
    await waitFor(() => expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument());
  });

  it('keeps a conflicting secondary edit open with the reload guidance', async () => {
    const z = seedSecondary();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    await waitFor(() => expect(edit).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(edit);
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, () => HttpResponse.json({ error: { key: 'precondition_failed', message: 'resourceVersion changed' } }, { status: 412 })));
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    expect(await within(section).findByText('The zone changed since this page loaded; reload and edit again.')).toBeInTheDocument();
    expect(within(section).getByRole('textbox', { name: 'Primary servers 1' })).toHaveValue('ns1.example.com');
  });

  it.each(['file:/objects.d/secondary.yaml', 'kubernetes'])('keeps %s secondary configuration read-only', async (owner) => {
    const z = seedSecondary();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership(owner)} />);
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    expect(edit).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(edit);
    expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument();
    expect(section).toHaveTextContent('ns1.example.com');
  });

  it('keeps secondary settings read-only without zones.write', async () => {
    const z = seedSecondary();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />, { user: 'viewer' });
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    expect(edit).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(edit);
    expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument();
  });

  it('edits a forward zone and saves it', async () => {
    signIn('admin');
    const a = endpoints(new ApiClient({ baseUrl: API, csrf: () => 'csrf-mock-token' }));
    await a.objects.create('dnszones', { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: 'corp-example' }, spec: { zoneName: 'corp.example.', type: 'forward', forward: {} } });
    renderWithApp(<ZoneSettingsSection zone="corp-example" own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    fireEvent.click(within(section).getByRole('button', { name: /Edit/ }));
    expect(await within(section).findByText('DNSSEC validation', { exact: true })).toBeInTheDocument();
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    await waitFor(() => expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument());
  });
});

describe('NOTIFY options in zone settings', () => {
  function seedNotify() {
    signIn('admin');
    const z = {
      apiVersion: 'kudurru.io/v1alpha1' as const, kind: 'DNSZone' as const,
      metadata: { name: 'notify.example', resourceVersion: '12', labels: { owner: 'dns-team' } },
      spec: {
        zoneName: 'notify.example.', type: 'primary' as const,
        policy: { notify: 'specified' as const, notifyTargets: ['192.0.2.1'], transferTSIGKeyRefs: ['transfer.'], futureNotifyOption: 'retained', notifyTSIGKeyRef: 'notify.', notifySource: '127.0.0.2' },
      },
    };
    mockDB().zones.push(z);
    return z;
  }

  it.each([false, true])('saves and clears explicit choices (clear=%s) with If-Match and unrelated policy preserved', async (clear) => {
    const z = seedNotify();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    expect(within(section).getByText('notify.')).toBeInTheDocument();
    fireEvent.click(within(section).getByRole('button', { name: 'Edit Zone settings' }));
    const key = within(section).getByRole('textbox', { name: /^NOTIFY TSIG key/ });
    const source = within(section).getByRole('textbox', { name: /^NOTIFY source address/ });
    expect(key).toHaveValue('notify.');
    expect(source).toHaveValue('127.0.0.2');
    expect(key).toHaveAccessibleDescription(/Empty uses the first transfer key/);
    expect(source).toHaveAccessibleDescription(/Empty inherits notify.source/);
    fireEvent.change(key, { target: { value: clear ? '' : 'other-notify.' } });
    fireEvent.change(source, { target: { value: clear ? '' : '::1' } });
    const save = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, async ({ request }) => {
      const body = await request.json();
      save(request.headers.get('If-Match'), body);
      return HttpResponse.json(body);
    }));
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    const expectedPolicy = {
      notify: 'specified', notifyTargets: ['192.0.2.1'], transferTSIGKeyRefs: ['transfer.'], futureNotifyOption: 'retained',
      ...(clear ? {} : { notifyTSIGKeyRef: 'other-notify.', notifySource: '::1' }),
    };
    await waitFor(() => expect(save).toHaveBeenCalledExactlyOnceWith('12', { ...z, spec: { ...z.spec, policy: expectedPolicy } }));
  });

  it.each([['NOTIFY source address', '::'], ['NOTIFY TSIG key', 'a..b']])('keeps invalid %s inline without submitting', async (label, value) => {
    const z = seedNotify();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    fireEvent.click(within(section).getByRole('button', { name: 'Edit Zone settings' }));
    const source = within(section).getByRole('textbox', { name: new RegExp(`^${label}`) });
    const save = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, () => { save(); return HttpResponse.json(z); }));
    fireEvent.change(source, { target: { value } });
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    expect(source).toHaveAttribute('aria-invalid', 'true');
    expect(save).not.toHaveBeenCalled();
    expect(within(section).getByRole('button', { name: 'Save Zone settings' })).toBeInTheDocument();
  });

  it('keeps refused current edits open and never replays the PUT', async () => {
    const z = seedNotify();
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />);
    const section = await screen.findByTestId('section-zone-settings');
    fireEvent.click(within(section).getByRole('button', { name: 'Edit Zone settings' }));
    fireEvent.change(within(section).getByRole('textbox', { name: /^NOTIFY source address/ }), { target: { value: '::1' } });
    const save = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, () => { save(); return HttpResponse.json({ error: { key: 'precondition_failed', message: 'resourceVersion changed' } }, { status: 412 }); }));
    fireEvent.click(within(section).getByRole('button', { name: 'Save Zone settings' }));
    expect(await within(section).findByText('The zone changed since this page loaded; reload and edit again.')).toBeInTheDocument();
    expect(save).toHaveBeenCalledTimes(1);
    expect(within(section).getByRole('textbox', { name: /^NOTIFY source address/ })).toHaveValue('::1');
  });

  it('does not expose edit controls to a viewer', async () => {
    const z = seedNotify();
    const save = vi.fn();
    server.use(http.put(`${API}/objects/dnszones/${z.metadata.name}`, () => { save(); return HttpResponse.json({}); }));
    renderWithApp(<ZoneSettingsSection zone={z.metadata.name} own={ownership('api')} />, { user: 'viewer' });
    const section = await screen.findByTestId('section-zone-settings');
    const edit = within(section).getByRole('button', { name: 'Edit Zone settings' });
    expect(edit).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(edit);
    expect(within(section).queryByRole('textbox', { name: /^NOTIFY source address/ })).not.toBeInTheDocument();
    expect(within(section).queryByRole('textbox', { name: /^NOTIFY TSIG key/ })).not.toBeInTheDocument();
    expect(within(section).queryByRole('button', { name: 'Save Zone settings' })).not.toBeInTheDocument();
    expect(save).not.toHaveBeenCalled();
  });
});
