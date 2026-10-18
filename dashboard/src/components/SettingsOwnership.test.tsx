import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { qk } from '../api/context';
import { mockDB } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import UpstreamsPage from '../pages/UpstreamsPage';

describe('upstreams ownership', () => {
  it('adopts with confirmation and enables upstream editing without changing values', async () => {
    const db = mockDB(); db.settingsOwner = 'gitops';
    const before = structuredClone(db.settings.upstreams);
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const adopt = await screen.findByRole('button', { name: 'Adopt into UI management' });
    expect(screen.getByRole('button', { name: 'New group' })).toBeDisabled();
    fireEvent.click(adopt);
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Adopt upstreams' }));
    await screen.findByText('Managed in the UI');
    expect(screen.getByRole('button', { name: 'New group' })).toBeEnabled();
    expect(db.settings.upstreams).toEqual(before);
    expect(db.settingsOwner).toBe('ui');
  });

  it('exports proposed changes without saving and requires confirmation before release', async () => {
    const db = mockDB(); db.settingsOwner = 'ui'; db.settingsConflict = true;
    const before = structuredClone(db.settings.upstreams);
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    expect(await screen.findByText('GitOps also declares upstreams')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Generate YAML' }));
    fireEvent.click(screen.getByRole('checkbox', { name: 'Edit proposed upstreams as JSON' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Proposed upstreams (JSON)' }), { target: { value: JSON.stringify({ groups: { default: { servers: ['192.0.2.1:53'] } } }) } });
    fireEvent.click(screen.getByRole('button', { name: 'Generate preview' }));
    await waitFor(() => expect(document.querySelector('pre')?.textContent).toContain('192.0.2.1:53'));
    expect(db.settings.upstreams).toEqual(before);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByRole('button', { name: 'Return to GitOps' }));
    fireEvent.click(screen.getByRole('button', { name: 'Generate preview' }));
    const release = await screen.findByRole('button', { name: 'Release to GitOps' });
    expect(release).toBeDisabled();
    fireEvent.click(screen.getByRole('checkbox', { name: 'I committed and applied these Helm values' }));
    fireEvent.click(release);
    await screen.findByText('Managed by GitOps');
    expect(db.settingsOwner).toBe('gitops');
    expect(db.settings.upstreams).toEqual(before);
  });

  it('shows ownership to viewers and prevents them from adopting', async () => {
    mockDB().settingsOwner = 'gitops';
    renderWithApp(<UpstreamsPage />, { route: '/upstreams', user: 'viewer' });
    expect(await screen.findByRole('button', { name: 'Adopt into UI management' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Generate YAML' })).toBeEnabled();
  });

  it('keeps the reviewed adoption revision when settings refresh during confirmation', async () => {
    const db = mockDB(); db.settingsOwner = 'gitops';
    const { client } = renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    fireEvent.click(await screen.findByRole('button', { name: 'Adopt into UI management' }));
    const dialog = await screen.findByRole('dialog');
    db.settingsRevision = (db.settingsRevision ?? 0) + 1;
    await act(async () => { await client.invalidateQueries({ queryKey: qk.settings }); });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Adopt upstreams' }));
    expect(await within(dialog).findByText(/Settings changed\. Refresh and review\./)).toBeInTheDocument();
    expect(db.settingsOwner).toBe('gitops');
    expect(screen.getByRole('button', { name: 'New group' })).toBeDisabled();
  });
});
