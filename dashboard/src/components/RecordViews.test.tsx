// Client-group record views in the record editor and the record list (D403).
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import type { ClientGroup, ZoneRecord } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { getSelector, optionLabels, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import ZoneDetailPage, { listsServed } from '../pages/ZoneDetailPage';
import { isGroupView, RecordEditor, recordSetFor, remapView, viewError, viewName, viewNamespaceError, viewOptions } from './RecordEditor';

const group = (name: string, namespace = 'default'): ClientGroup => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'ClientGroup', metadata: { name, namespace }, spec: {} });
const GROUPS = [group('kids'), group('lan'), group('lab', 'apps')];

function setup(props: { initial?: ZoneRecord; signed?: boolean; listsServed?: boolean } = {}) {
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

  it('says on a server that lists what it serves that a public record is stored but not listed', () => {
    setup({ listsServed: true });
    pick(getSelector('Served to'), 'Published only (public)');
    expect(screen.getByText('A public record is stored but not listed here')).toBeInTheDocument();
    pick(getSelector('Served to'), 'Client group kids');
    expect(screen.queryByText('A public record is stored but not listed here')).not.toBeInTheDocument();
    expect(screen.getByText(/a view it refuses is stored but does not appear in the record list/)).toBeInTheDocument();
  });

  it('says nothing of unlisted public records where the list is the desired data', () => {
    setup();
    pick(getSelector('Served to'), 'Published only (public)');
    expect(screen.queryByText('A public record is stored but not listed here')).not.toBeInTheDocument();
  });

  it('keeps the chosen group when the record set gains its namespace, and refuses another namespace', async () => {
    const onSubmit = setup();
    fireEvent.change(field(/^Name/), { target: { value: 'www' } });
    fireEvent.change(field(/IPv4 address/), { target: { value: '10.0.0.10' } });
    pick(getSelector('Served to'), 'Client group apps/lab');
    // Another namespace: the save is refused instead of storing view "lab" in default.
    fireEvent.change(field(/^Record set/), { target: { value: 'default/api-x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    expect((await screen.findAllByText(/Client group apps\/lab is in namespace apps, but record set default\/api-x is in default/)).length).toBeGreaterThan(0);
    expect(onSubmit).not.toHaveBeenCalled();
    // The group's own namespace: the picker keeps naming the listed group.
    fireEvent.change(field(/^Record set/), { target: { value: 'apps/api-x' } });
    expect(getSelector('Served to')).toHaveTextContent('Client group apps/lab');
    expect(optionLabels(getSelector('Served to'))).toEqual(['Every client (default)', 'Published only (public)', 'Client group apps/lab']);
    fireEvent.click(screen.getByRole('button', { name: 'Add record' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ kudurru: { view: 'lab' } }), 'apps/api-x');
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
    expect(remapView('apps/lab', 'api-x', 'apps/api-x')).toBe('lab');
    expect(remapView('apps/lab', 'api-x', 'default/api-x')).toBe('apps/lab');
    expect(remapView('lab', 'apps/api-x', 'api-x')).toBe('apps/lab');
    expect(remapView('lab', 'apps/api-x', 'apps/other')).toBe('lab');
    expect(remapView('public', 'api-x', 'apps/api-x')).toBe('public');
    expect(viewNamespaceError('apps/lab', 'default/api-x')).toMatch(/namespace apps/);
    expect(viewNamespaceError('apps/lab', 'apps/api-x')).toBeUndefined();
    expect(viewNamespaceError('apps/lab', 'api-x')).toBeUndefined();
    expect(viewNamespaceError('lab', 'default/api-x')).toBeUndefined();
    expect(listsServed([])).toBe(false);
    expect(listsServed([{ recordSet: '', dnsName: '@', recordType: 'SOA', targets: [] }])).toBe(true);
    expect(listsServed([{ recordSet: 'default/a', dnsName: 'www', recordType: 'A', targets: [] }])).toBe(false);
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

  it('on a served list, keeps views of one name in two namespaces apart and deletes one namespace’s only', async () => {
    const soa: ZoneRecord = { recordSet: '', dnsName: '@', recordType: 'SOA', targets: ['ns1.example.internal. hostmaster.example.internal. 1 3600 600 86400 300'] };
    const nas = (namespace: string, target: string): ZoneRecord => ({ recordSet: '', namespace, dnsName: 'nas.example.internal', recordType: 'A', targets: [target], kudurru: { view: 'lan' } });
    server.use(http.get(`${API}/zones/:zone/records`, () => HttpResponse.json({ items: [soa, nas('default', '10.0.0.20'), nas('kids', '10.9.0.20')], took_ms: 1 })));
    const deletes: URLSearchParams[] = [];
    server.use(
      http.delete(`${API}/zones/:zone/records/:name/:type`, ({ request }) => {
        deletes.push(new URL(request.url).searchParams);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    expect(await screen.findByText('This list shows what this node serves')).toBeInTheDocument();
    const own = await screen.findByText('view lan');
    const kids = screen.getByText('view kids/lan');
    expect(own.closest('a')).toHaveAttribute('href', '/groups/default/lan');
    expect(kids.closest('a')).toHaveAttribute('href', '/groups/kids/lan');
    const del = within(kids.closest('tr')!).getByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent('served to client group kids/lan in every record set of namespace kids');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete record' }));
    await waitFor(() => expect(deletes).toHaveLength(1));
    expect(deletes[0]!.get('view')).toBe('lan');
    expect(deletes[0]!.get('namespace')).toBe('kids');
    expect(deletes[0]!.get('recordSet')).toBeNull();
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
