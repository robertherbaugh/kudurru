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

  it('counts enabled sources and links each row to the list page', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const family = await screen.findByTestId('row-family');
    expect(within(family).getByText('3 of 4 enabled')).toBeInTheDocument();
    expect(within(screen.getByTestId('row-stevenblack')).getByText('1 source')).toBeInTheDocument();
    expect(within(family).getByRole('link', { name: 'family' })).toHaveAttribute('href', '/lists/default/family');
    // File-managed lists open the same page (read-only there).
    expect(within(screen.getByTestId('row-regex-trackers')).getByRole('link', { name: 'Open' })).toHaveAttribute('href', '/lists/default/regex-trackers');
  });

  it('opens the wizard for a new list', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    const link = await screen.findByRole('link', { name: 'New list' });
    expect(link).toHaveAttribute('href', '/lists/new');
    fireEvent.click(link);
    expect(window.location.pathname).toBe('/lists/new');
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

  it('adds domains to ui-deny with a typed choice and reports per-item results', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    await clickWhenEnabled('Add domains');
    const domains = await screen.findByRole('textbox', { name: /^Domains/ });
    fireEvent.change(domains, { target: { value: 'not_a..domain' } });
    expect(await screen.findByText(/is not a domain name/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add 1' })).toBeDisabled();
    fireEvent.change(domains, { target: { value: 'ads.example.com\ntracking.example-shop.com' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add 2' }));
    expect(await screen.findByText('1 added, 1 rejected.')).toBeInTheDocument();
    expect(mockDB().lists.find((l) => l.metadata.name === 'ui-deny')?.spec.sources[0]?.inline).toContain('ads.example.com');
  });

  it('adds an allow regex through the Type select', async () => {
    renderWithApp(<ListsPage />, { route: '/lists' });
    await clickWhenEnabled('Add domains');
    fireEvent.change(await screen.findByRole('combobox', { name: /^Type/ }), { target: { value: 'allow:regex' } });
    const box = screen.getByRole('textbox', { name: /^Expressions/ });
    fireEvent.change(box, { target: { value: '(' } });
    expect(await screen.findByText(/Not a valid expression/)).toBeInTheDocument();
    fireEvent.change(box, { target: { value: '^cdn[0-9]+\\.example\\.com$' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add 1' }));
    expect(await screen.findByText('1 added, 0 rejected.')).toBeInTheDocument();
    expect(mockDB().lists.find((l) => l.metadata.name === 'ui-allow')?.spec.sources[0]?.inline).toContain('/^cdn[0-9]+\\.example\\.com$/');
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
