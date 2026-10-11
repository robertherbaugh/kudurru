import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { cleanSpec, validateFields } from '../components/ObjectForm';
import { mockDB } from '../mocks/handlers';
import { SCHEMAS } from '../lib/objectSchemas';
import { renderWithApp } from '../test/utils';
import ListsPage, { inlineOnly } from './ListsPage';

/** Clicks a button once the session has loaded and enabled it. */
async function clickWhenEnabled(name: string): Promise<void> {
  const btn = await screen.findByRole('button', { name });
  await waitFor(() => expect(btn).not.toHaveAttribute('aria-disabled', 'true'));
  fireEvent.click(btn);
}

describe('ListsPage', () => {
  it('shows health, managed-by badges and keeps foreign objects out of the selection', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const file = await screen.findByTestId('row-regex-trackers');
    expect(within(file).getByLabelText('Managed by file:objects.d/lists.yaml')).toBeInTheDocument();
    expect(within(file).getByRole('checkbox', { name: 'Select regex-trackers' })).toBeDisabled();
    expect(within(screen.getByTestId('row-oisd-small')).getByText('unavailable, cached')).toBeInTheDocument();
    expect(within(screen.getByTestId('row-ui-deny')).getByText('invalid')).toBeInTheDocument();
    expect(within(screen.getByTestId('row-starter')).getByText('OK')).toBeInTheDocument();
  });

  it('says "inline" under Updated for lists without anything to fetch', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const starter = await screen.findByTestId('row-starter');
    expect(within(starter).getByText('inline')).toBeInTheDocument();
    expect(inlineOnly([{ inline: ['a.example'] }])).toBe(true);
    expect(inlineOnly([{ inline: ['a.example'] }, { url: 'https://x/y' }])).toBe(false);
  });

  it('opens a file-managed list read-only with its owner', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const row = await screen.findByTestId('row-regex-trackers');
    fireEvent.click(within(row).getByRole('button', { name: 'View' }));
    expect(await screen.findByText('Read-only: managed by file:objects.d/lists.yaml')).toBeInTheDocument();
    expect(screen.getByText('Managed by objects.d/lists.yaml; edit the file on the host.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('batch-deletes the selected lists after confirmation', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const row = await screen.findByTestId('row-stevenblack');
    fireEvent.click(within(row).getByRole('checkbox', { name: 'Select stevenblack' }));
    fireEvent.click(within(screen.getByTestId('row-ui-deny')).getByRole('checkbox', { name: 'Select ui-deny' }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete 2 selected' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(mockDB().lists.map((l) => l.metadata.name)).not.toContain('stevenblack'));
    expect(mockDB().lists.map((l) => l.metadata.name)).not.toContain('ui-deny');
    expect(await screen.findByText('Deleted 2 lists')).toBeInTheDocument();
  });

  it('creates a list from the generated form, with validation first', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    await clickWhenEnabled('New list');
    const objectName = (await screen.findAllByRole('textbox', { name: /^Name/ }))[0]!;
    fireEvent.change(objectName, { target: { value: 'Bad Name' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^URL/ }), { target: { value: 'ftp://x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create list' }));
    expect((await screen.findAllByText('An http(s) URL')).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/must start and end with a letter/).length).toBeGreaterThan(0);
    fireEvent.change(objectName, { target: { value: 'hagezi' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^URL/ }), { target: { value: 'https://example.com/hagezi.txt' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^Comment/ }), { target: { value: 'multi pro' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create list' }));
    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'hagezi')).toBeDefined());
    const created = mockDB().lists.find((l) => l.metadata.name === 'hagezi');
    expect(created?.spec.sources).toEqual([{ name: 'main', url: 'https://example.com/hagezi.txt', format: 'auto' }]);
    expect(created?.metadata.annotations?.['kudurru.io/comment']).toBe('multi pro');
  });

  it('adds domains to ui-deny and reports per-item results', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    await clickWhenEnabled('Add domains');
    fireEvent.change(await screen.findByRole('textbox', { name: /^Domains/ }), { target: { value: 'ads.example.com\ntracking.example-shop.com' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add 2' }));
    expect(await screen.findByText('1 added, 1 rejected.')).toBeInTheDocument();
  });
});

describe('generated form helpers', () => {
  it('validates nested items by dotted path', () => {
    const errs = validateFields(SCHEMAS.blocklists.fields, { mode: 'block', sources: [{ url: 'https://x.example/l.txt' }, { url: 'ftp://x' }], customIP: { v4: '300.1.1.1' } });
    expect(errs).toEqual({ 'sources.1.url': 'An http(s) URL', 'customIP.v4': 'Not an IPv4 address' });
  });

  it('drops empty values before sending', () => {
    expect(
      cleanSpec(SCHEMAS.blocklists.fields, {
        mode: 'block',
        sources: [{ name: 'main', url: 'https://x.example/l.txt', inline: ['', ' '] }, {}],
        refreshInterval: '  ',
        customIP: { v4: '', v6: '' },
      }),
    ).toEqual({ mode: 'block', sources: [{ name: 'main', url: 'https://x.example/l.txt' }] });
  });
});
