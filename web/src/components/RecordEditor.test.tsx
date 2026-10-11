import { fireEvent, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ZoneRecord } from '../api/types';
import { renderWithApp } from '../test/utils';
import { draftFrom, hasErrors, RecordEditor, validateRecord } from './RecordEditor';

function setup(initial?: ZoneRecord) {
  const onSubmit = vi.fn<(ep: unknown, rs: string) => Promise<void>>().mockResolvedValue(undefined);
  renderWithApp(<RecordEditor zoneName="example.internal." initial={initial} defaultRecordSet="api-example-internal" onSubmit={onSubmit} onCancel={vi.fn()} />, {
    user: null,
  });
  return onSubmit;
}

const field = (name: RegExp | string) => screen.getByRole('textbox', { name });

describe('RecordEditor', () => {
  it('blocks submitting an invalid A record and shows the field error', async () => {
    const onSubmit = setup();
    fireEvent.change(field(/^Name/), { target: { value: 'nas' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '192.168.1.300' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    expect((await screen.findAllByText('Not an IPv4 address')).length).toBeGreaterThan(0);
    expect(screen.getByText('Fix the highlighted fields before saving.')).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('rejects an owner outside the zone and a bad TTL', async () => {
    const onSubmit = setup();
    fireEvent.change(field(/^Name/), { target: { value: 'www.other.com.' } });
    fireEvent.change(screen.getByRole('combobox', { name: 'TTL' }), { target: { value: '__custom__' } });
    fireEvent.change(field('TTL (custom)'), { target: { value: '-5' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '192.0.2.1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    expect((await screen.findAllByText('Not inside example.internal.')).length).toBeGreaterThan(0);
    expect(screen.getAllByText('Seconds, 0 to 2147483647').length).toBeGreaterThan(0);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('composes a typed MX record with two targets into an Endpoint', async () => {
    const onSubmit = setup();
    fireEvent.change(screen.getByRole('combobox', { name: 'Type' }), { target: { value: 'MX' } });
    fireEvent.change(field(/^Name/), { target: { value: '@' } });
    fireEvent.change(screen.getByRole('combobox', { name: 'TTL' }), { target: { value: '3600' } });
    fireEvent.change(field(/Preference/), { target: { value: '10' } });
    fireEvent.change(field(/Mail server/), { target: { value: 'mx1.example.internal' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add target' }));
    const prefs = screen.getAllByRole('textbox', { name: /Preference/ });
    const hosts = screen.getAllByRole('textbox', { name: /Mail server/ });
    fireEvent.change(prefs[1]!, { target: { value: '20' } });
    fireEvent.change(hosts[1]!, { target: { value: 'mx2.example.internal.' } });
    fireEvent.change(field(/Comment/), { target: { value: 'mail' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith(
      {
        dnsName: 'example.internal',
        recordType: 'MX',
        targets: ['10 mx1.example.internal.', '20 mx2.example.internal.'],
        recordTTL: 3600,
        kudurru: { comment: 'mail' },
      },
      'api-example-internal',
    );
  });

  it('opens an existing SRV record field by field and keeps name/type fixed', () => {
    setup({ recordSet: 'svc', dnsName: '_sip._udp.example.internal', recordType: 'SRV', targets: ['10 5 5060 sip.example.internal.'], recordTTL: 300 });
    expect(field(/^Name/)).toHaveValue('_sip._udp');
    expect(field(/Port/)).toHaveValue('5060');
    expect(field(/Target host/)).toHaveValue('sip.example.internal.');
    expect(screen.getByRole('combobox', { name: 'Type' })).toBeDisabled();
  });

  it('shows the server error verbatim', async () => {
    const onSubmit = setup();
    onSubmit.mockRejectedValueOnce(new Error('spec.endpoints[0].targets[0]: rdata does not parse'));
    fireEvent.change(field(/^Name/), { target: { value: 'nas' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '192.0.2.40' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    expect(await screen.findByText('spec.endpoints[0].targets[0]: rdata does not parse')).toBeInTheDocument();
  });
});

describe('validateRecord', () => {
  it('allows only one CNAME target', () => {
    const e = validateRecord('example.internal', 'www', '', { type: 'CNAME', targets: [{ target: 'a.example.' }, { target: 'b.example.' }] });
    expect(e.targets[1]).toEqual({ target: 'A CNAME has exactly one target' });
    expect(hasErrors(e)).toBe(true);
  });

  it('opens unparseable targets as raw rdata', () => {
    expect(draftFrom({ recordSet: 'x', dnsName: 'a.example', recordType: 'MX', targets: ['nonsense'] })).toEqual({
      type: 'raw',
      targets: [{ rrtype: 'MX', rdata: 'nonsense' }],
    });
  });
});
