import { SideNav } from '@astryxdesign/core/SideNav';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { qk } from '../api/context';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { API, renderWithApp, testQueryClient } from '../test/utils';
import { LicenseBanner } from './LicenseBanner';
import { SidebarStatus } from './SidebarStatus';

describe('sidebar runtime metadata', () => {
  it('uses the serving node version in a mixed cluster and shares existing license data', async () => {
    mockDB().nodes = [
      { name: 'peer', self: false, leader: true, version: 'v9.9.9', ready: true },
      { name: 'local', self: true, leader: false, version: 'v0.1.0-rc.2-12-gdeadbee-dirty', ready: true },
    ];
    window.localStorage.setItem('kudurru.scope', JSON.stringify({ scope: 'node', node: 'peer' }));
    mockDB().license.plan = 'enterprise';
    const versionRequest = vi.fn(() => HttpResponse.json({ version: 'v9.9.9' }));
    const licenseRequest = vi.fn(() => HttpResponse.json(mockDB().license));
    server.use(http.get(`${API}/version`, versionRequest), http.get(`${API}/license`, licenseRequest));
    renderWithApp(<><SidebarStatus /><LicenseBanner /></>);
    const metadata = screen.getByRole('group', { name: 'Running version and license' });
    expect(await within(metadata).findByText('v0.1.0-rc.2')).toBeInTheDocument();
    expect(await within(metadata).findByText('Enterprise')).toBeInTheDocument();
    expect(versionRequest).not.toHaveBeenCalled();
    expect(licenseRequest).toHaveBeenCalledTimes(1);
  });

  it('does not substitute a peer version or guess a plan when server data is unavailable', async () => {
    mockDB().nodes = [{ name: 'peer', self: false, leader: true, version: 'v9.9.9', ready: true }];
    server.use(http.get(`${API}/license`, () => HttpResponse.json({ error: { key: 'forbidden', message: 'Permission denied' } }, { status: 403 })));
    const client = testQueryClient();
    // A failed refresh retains cached data in TanStack Query. It must not
    // advertise yesterday's paid plan as the current effective license.
    client.setQueryData(qk.license, { ...mockDB().license, plan: 'enterprise' }, { updatedAt: 1 });
    renderWithApp(<SidebarStatus />, { client });
    await waitFor(() => expect(client.getQueryState(qk.license)?.status).toBe('error'));
    const metadata = screen.getByRole('group', { name: 'Running version and license' });
    expect(metadata).toHaveTextContent('–');
    expect(metadata).not.toHaveTextContent(/v9\.9\.9|Community|Personal|Enterprise/);
  });

  it('hides metadata in a collapsed nav and restores it on expansion', async () => {
    renderWithApp(<SideNav collapsible={{ defaultIsCollapsed: true }} footer={<SidebarStatus />}>{null}</SideNav>);
    expect(screen.queryByRole('group', { name: 'Running version and license' })).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    const metadata = await screen.findByRole('group', { name: 'Running version and license' });
    expect(await within(metadata).findByText('v0.1.0-rc.1')).toBeInTheDocument();
    expect(await within(metadata).findByText('Personal')).toBeInTheDocument();
  });
});
