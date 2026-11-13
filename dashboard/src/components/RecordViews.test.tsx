// Client-group record views in the record editor and the record list (D403).
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { ClientGroup, ZoneRecord } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { getSelector, optionLabels, pick } from '../test/controls';
import { renderWithApp } from '../test/utils';
import ZoneDetailPage from '../pages/ZoneDetailPage';
import { isGroupView, RecordEditor, recordSetFor, viewError, viewName, viewOptions } from './RecordEditor';

const group = (name: string, namespace = 'default'): ClientGroup => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'ClientGroup', metadata: { name, namespace }, spec: {} });
const GROUPS = [group('kids'), group('lan'), group('lab', 'apps')];

function setup(props: { initial?: ZoneRecord; signed?: boolean } = {}) {
  const onSubmit = vi.fn<(ep: unknown, rs: string) => Promise<void>>().mockResolvedValue(undefined);
  renderWithApp(<RecordEditor zoneName="example.internal." defaultRecordSet="api-example-internal" groups={GROUPS} onSubmit={onSubmit} onCancel={vi.fn()} {...props} />, { user: null });
  return onSubmit;
}

const field = (name: RegExp | string) => screen.getByRole('textbox', { name });

describe('record editor views', () => {
  it('offers the default data, public and every ClientGroup, and says how a group view is served', () => {
    setup();
    const picker = getSelector('Served to');
    expect(optionLabels(picker)).toEqual(['Every client (default)', 'Published only (public)', 'Client group apps/lab', 'Client group kids', 'Client group lan']);
    expect(screen.queryByText('How a client-group view is served')).not.toBeInTheDocument();
    pick(picker, 'Client group lan');
    expect(screen.getByText('How a client-group view is served')).toBeInTheDocument();
    expect(screen.getByText(/carry the default data only, so a secondary never receives the view/)).toBeInTheDocument();
    expect(screen.getByText(/dnsdist edge bypasses its authoritative and resolver packet caches/)).toBeInTheDocument();
    expect(screen.getByText(/not served if the zone is DNSSEC-signed/)).toBeInTheDocument();
  });

  it('writes a group view with kudurru.view into the group’s namespace', async () => {
    const onSubmit = setup();
    fireEvent.change(field(/^Name/), { target: { value: 'www' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '10.0.0.10' } });
    pick(getSelector('Served to'), 'Client group apps/lab');
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ dnsName: 'www.example.internal', kudurru: { view: 'lab' } }), 'apps/api-example-internal');
  });

  it('writes public and the default data without moving the record set', async () => {
    const onSubmit = setup();
    fireEvent.change(field(/^Name/), { target: { value: 'www' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '203.0.113.10' } });
    pick(getSelector('Served to'), 'Published only (public)');
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenLastCalledWith(expect.objectContaining({ kudurru: { view: 'public' } }), 'api-example-internal');
  });

  it('refuses the types a view cannot hold before sending', async () => {
    const onSubmit = setup();
    pick(getSelector('Type'), /^NS/);
    fireEvent.change(field(/^Name$/), { target: { value: 'sub' } });
    fireEvent.change(field(/Name server/), { target: { value: 'ns1.example.internal.' } });
    pick(getSelector('Served to'), 'Client group kids');
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    expect((await screen.findAllByText(/holds leaf data only; NS records stay in the default data/)).length).toBeGreaterThan(0);
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('warns in a signed zone that group views are not served', () => {
    setup({ signed: true });
    pick(getSelector('Served to'), 'Client group kids');
    expect(screen.getByText('This zone is signed: client-group views are not served')).toBeInTheDocument();
    expect(screen.getByText(/refuses the endpoint as Invalid and the group gets the default data/)).toBeInTheDocument();
  });

  it('keeps an existing record’s view fixed and offers only its namespace’s groups', () => {
    setup({ initial: { recordSet: 'default/api-example-internal', namespace: 'default', dnsName: 'www.example.internal', recordType: 'A', targets: ['10.0.0.10'], kudurru: { view: 'kids' } } });
    const picker = getSelector('Served to');
    expect(picker).toBeDisabled();
    expect(picker).toHaveTextContent('Client group kids');
    expect(optionLabels(picker)).not.toContain('Client group apps/lab');
  });

  it('helpers follow the server rules', () => {
    expect(viewOptions(GROUPS, 'apps', '').map((o) => o.value)).toEqual(['', 'public', 'lab']);
    expect(viewOptions([], 'default', 'gone').at(-1)?.label).toBe('Client group gone');
    expect(viewName('apps/lab')).toBe('lab');
    expect(recordSetFor('api-x', 'apps/lab')).toBe('apps/api-x');
    expect(recordSetFor('default/api-x', 'apps/lab')).toBe('default/api-x');
    expect(recordSetFor('api-x', 'public')).toBe('api-x');
    expect(isGroupView('')).toBe(false);
    expect(isGroupView('public')).toBe(false);
    expect(viewError('kids', 'CNAME', '@')).toMatch(/apex/);
    expect(viewError('kids', 'DNSKEY', 'www')).toMatch(/leaf data/);
    expect(viewError('public', 'NS', 'www')).toBeUndefined();
    expect(viewError('kids', 'A', 'www')).toBeUndefined();
  });
});

describe('record list views', () => {
  function seedView() {
    const rs = mockDB().recordSets.find((r) => r.metadata.name === 'api-example-internal')!;
    const first = rs.spec.endpoints[0]!;
    rs.spec.endpoints.push({ ...first, targets: ['10.0.0.99'], kudurru: { view: 'kids' } });
    return first;
  }

  it('shows each row’s view, links the group, and deletes one view only', async () => {
    const first = seedView();
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    const badge = await screen.findByText('view kids');
    expect(badge.closest('a')).toHaveAttribute('href', '/groups/default/kids');
    const row = badge.closest('tr')!;
    const del = within(row).getByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent('served to client group kids');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete record' }));
    await waitFor(() => expect(screen.queryByText('view kids')).not.toBeInTheDocument());
    const rs = mockDB().recordSets.find((r) => r.metadata.name === 'api-example-internal')!;
    // The default data of the same name and type survives.
    expect(rs.spec.endpoints.some((e) => e.dnsName === first.dnsName && e.recordType === first.recordType && !e.kudurru?.view)).toBe(true);
  });

  it('adds a view next to the default data instead of replacing it', async () => {
    const rs = mockDB().recordSets.find((r) => r.metadata.name === 'api-example-internal')!;
    const before = rs.spec.endpoints.length;
    const a = rs.spec.endpoints.find((e) => e.recordType === 'A')!;
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    const add = await screen.findByRole('button', { name: 'Add record' });
    await waitFor(() => expect(add).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(add);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Name/ }), { target: { value: a.dnsName.replace(/\.example\.internal$/, '') } });
    fireEvent.change(within(dialog).getByRole('textbox', { name: /IPv4 address/ }), { target: { value: '10.0.0.77' } });
    await waitFor(() => expect(optionLabels(getSelector('Served to', dialog))).toContain('Client group kids'));
    pick(getSelector('Served to', dialog), 'Client group kids');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add record' }));
    await waitFor(() => expect(rs.spec.endpoints.length).toBe(before + 1));
    expect(rs.spec.endpoints.find((e) => e.dnsName === a.dnsName && e.recordType === 'A' && !e.kudurru?.view)?.targets).toEqual(a.targets);
    expect(rs.spec.endpoints.at(-1)).toMatchObject({ dnsName: a.dnsName, kudurru: { view: 'kids' }, targets: ['10.0.0.77'] });
  });
});
