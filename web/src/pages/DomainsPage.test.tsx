import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { BlockList } from '../api/types';
import { domainError, splitDomains } from '../components/lists/AddDomainsDialog';
import { mockDB } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import DomainsPage, { domainRows } from './DomainsPage';

const uiDeny = () => mockDB().lists.find((l) => l.metadata.name === 'ui-deny');

describe('DomainsPage', () => {
  it('lists every allow and deny entry with its type and list', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    const allow = await screen.findByTestId('domain-s.youtube.com');
    expect(within(allow).getByText('allow')).toBeInTheDocument();
    expect(within(allow).getByText('exact')).toBeInTheDocument();
    expect(within(allow).getByRole('link', { name: 'ui-allow' })).toHaveAttribute('href', '/lists/default/ui-allow');
    const deny = screen.getByTestId('domain-tracking.example-shop.com');
    expect(within(deny).getByText('deny')).toBeInTheDocument();
    // Only the UI lists, not the big ones.
    expect(screen.queryByTestId('domain-tiktokv.com')).toBeNull();
    expect(within(screen.getByTestId('ui-list-ui-allow')).getByText('2 entries')).toBeInTheDocument();
  });

  it('filters by type and text', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    await screen.findByTestId('domain-s.youtube.com');
    fireEvent.change(screen.getByRole('combobox', { name: /^Type/ }), { target: { value: 'deny:exact' } });
    await waitFor(() => expect(screen.queryByTestId('domain-s.youtube.com')).toBeNull());
    expect(screen.getByTestId('domain-tracking.example-shop.com')).toBeInTheDocument();
    expect(window.location.search).toBe('?type=deny%3Aexact');
    fireEvent.change(screen.getByRole('combobox', { name: /^Type/ }), { target: { value: '' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Search domains' }), { target: { value: 'dartsearch' } });
    await waitFor(() => expect(screen.queryByTestId('domain-s.youtube.com')).toBeNull());
    expect(screen.getByTestId('domain-clickserve.dartsearch.net')).toBeInTheDocument();
  });

  it('removes a domain after confirmation', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    const row = await screen.findByTestId('domain-tracking.example-shop.com');
    fireEvent.click(within(row).getByRole('button', { name: 'Remove' }));
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(uiDeny()?.spec.sources[0]?.inline).toEqual([]));
    await waitFor(() => expect(screen.queryByTestId('domain-tracking.example-shop.com')).toBeNull());
  });

  it('adds a deny regex with a typed form', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains?type=deny:regex' });
    const add = await screen.findByRole('button', { name: 'Add domains' });
    await waitFor(() => expect(add).not.toBeDisabled());
    fireEvent.click(add);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('combobox', { name: /^Type/ })).toHaveValue('deny:regex');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Expressions/ }), { target: { value: '^ads?[0-9]*\\.' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add 1' }));
    await waitFor(() => expect(uiDeny()?.spec.sources[0]?.inline).toContain('/^ads?[0-9]*\\./'));
    fireEvent.click(within(dialog).getAllByRole('button', { name: /^Close/ })[0]!);
    const row = await screen.findByTestId('domain-/^ads?[0-9]*\\./');
    expect(within(row).getByText('regex')).toBeInTheDocument();
  });

  it('turns a whole UI list off (entries have no switch of their own)', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    const sw = within(await screen.findByTestId('ui-list-ui-allow')).getByRole('switch', { name: 'ui-allow enabled' });
    await waitFor(() => expect(sw).not.toBeDisabled());
    fireEvent.click(sw);
    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'ui-allow')?.metadata.annotations?.['kudurru.io/enabled']).toBe('false'));
    expect(await within(screen.getByTestId('domain-s.youtube.com')).findByText('(disabled)')).toBeInTheDocument();
  });
});

describe('domain helpers', () => {
  it('validates exact and regex entries like the server', () => {
    expect(domainError('exact', 'ads.example.com')).toBeNull();
    expect(domainError('exact', '*.example.com')).toBeNull();
    expect(domainError('exact', 'Ads.Example.COM.')).toBeNull();
    expect(domainError('exact', 'bad..example')).toMatch(/not a domain name/);
    expect(domainError('regex', '/^ads\\./')).toBeNull();
    expect(domainError('regex', '(')).toMatch(/Not a valid expression/);
    expect(splitDomains('a.example, b.example\n c.example')).toEqual(['a.example', 'b.example', 'c.example']);
  });

  it('reads regex entries from /…/ and regex-format sources', () => {
    const l = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'BlockList',
      metadata: { name: 'ui-deny', namespace: 'default' },
      spec: { mode: 'block', sources: [{ name: 'ui', inline: ['a.example', '/^x/', '*.w.example'] }, { name: 're', format: 'regex', inline: ['^y'] }, { url: 'https://x/y' }] },
    } as BlockList;
    expect(domainRows([l]).map((r) => `${r.value}:${r.action}:${r.kind}:${r.source}${r.wildcard ? ':wild' : ''}`)).toEqual([
      'a.example:deny:exact:ui',
      '/^x/:deny:regex:ui',
      '*.w.example:deny:exact:ui:wild',
      '^y:deny:regex:re',
    ]);
  });
});
