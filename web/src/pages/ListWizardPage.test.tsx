import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { BlockList, Policy } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import ListWizardPage, { withListRef, wizardSources } from './ListWizardPage';

const next = () => fireEvent.click(screen.getByRole('button', { name: 'Next' }));

describe('ListWizardPage', () => {
  it('creates a block list from a well-known list and a URL and adds it to the default policy', async () => {
    renderWithApp(<ListWizardPage />, { route: '/lists/new' });
    // Type
    expect(await screen.findByRole('radio', { name: /Block list/ })).toBeChecked();
    next();
    // Sources: nothing chosen yet.
    expect(await screen.findByRole('group', { name: 'Well-known lists' })).toBeInTheDocument();
    next();
    expect(await screen.findByText(/Add at least one source/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: 'Steven Black Unified hosts' }));
    expect(screen.getAllByText('MIT').length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole('button', { name: 'Add a URL' }));
    fireEvent.change(screen.getByRole('textbox', { name: /^URL 1/ }), { target: { value: 'https://lists.example.net/adblock/extra.txt' } });
    expect(screen.getByRole('combobox', { name: /^Format of URL 1/ })).toHaveValue('adblock');
    expect(screen.getByText('2 sources chosen.')).toBeInTheDocument();
    next();
    // Options: a suggested name, validated; the default policy is preselected.
    const name = await screen.findByRole('textbox', { name: /^Name/ });
    expect(name).toHaveValue('my-block-list');
    fireEvent.change(name, { target: { value: 'Bad Name' } });
    next();
    expect(await screen.findAllByText(/must start and end with a letter/)).not.toHaveLength(0);
    fireEvent.change(name, { target: { value: 'starter' } });
    next();
    expect(await screen.findAllByText('A list named starter exists already')).not.toHaveLength(0);
    fireEvent.change(name, { target: { value: 'home-ads' } });
    expect(await screen.findByRole('checkbox', { name: 'default/default' })).toBeChecked();
    fireEvent.change(screen.getByRole('combobox', { name: /^Refresh interval/ }), { target: { value: '6h' } });
    next();
    // Review
    const sources = await screen.findByRole('table', { name: 'Sources to create' });
    expect(within(sources).getByText('stevenblack')).toBeInTheDocument();
    expect(within(sources).getByText('https://lists.example.net/adblock/extra.txt')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Create list' }));

    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'home-ads')).toBeDefined());
    const created = mockDB().lists.find((l) => l.metadata.name === 'home-ads');
    expect(created?.spec).toEqual({
      mode: 'block',
      refreshInterval: '6h',
      sources: [
        { name: 'stevenblack', url: 'https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts', format: 'hosts' },
        { name: 'lists-extra', url: 'https://lists.example.net/adblock/extra.txt', format: 'adblock' },
      ],
    });
    await waitFor(() => expect(mockDB().policies.find((p) => p.metadata.name === 'default')?.spec.blockListRefs).toContain('home-ads'));
    await waitFor(() => expect(window.location.pathname).toBe('/lists/default/home-ads'));
  });

  it('creates an allow list from pasted entries without well-known block lists', async () => {
    renderWithApp(<ListWizardPage />, { route: '/lists/new' });
    fireEvent.click(await screen.findByRole('radio', { name: /Allow list/ }));
    next();
    expect(await screen.findByRole('group', { name: 'List URLs' })).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Well-known lists' })).toBeNull();
    fireEvent.change(screen.getByRole('textbox', { name: /^Paste entries/ }), { target: { value: 'bad..name' } });
    next();
    expect(await screen.findByText(/Pasted entries: 1 invalid/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Paste entries/ }), { target: { value: 's.youtube.com\n*.cdn.example.com' } });
    next();
    const name = await screen.findByRole('textbox', { name: /^Name/ });
    expect(name).toHaveValue('my-allow-list');
    expect(screen.queryByRole('combobox', { name: /^Block response/ })).toBeNull();
    fireEvent.click(screen.getByRole('switch', { name: /Start enabled/ }));
    next();
    fireEvent.click(await screen.findByRole('button', { name: 'Create list' }));
    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'my-allow-list')).toBeDefined());
    const created = mockDB().lists.find((l) => l.metadata.name === 'my-allow-list');
    expect(created?.spec).toEqual({ mode: 'allow', sources: [{ name: 'custom', inline: ['s.youtube.com', '*.cdn.example.com'], format: 'auto' }] });
    expect(created?.metadata.annotations?.['kudurru.io/enabled']).toBe('false');
    await waitFor(() => expect(mockDB().policies.find((p) => p.metadata.name === 'default')?.spec.allowListRefs).toContain('my-allow-list'));
  });

  it('cancels back to the lists', async () => {
    renderWithApp(<ListWizardPage />, { route: '/lists/new' });
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(window.location.pathname).toBe('/lists');
  });
});

describe('wizard helpers', () => {
  it('names sources uniquely', () => {
    const base = { mode: 'block' as const, urls: [], paste: '', pasteFormat: 'auto' as const, name: '', nameTouched: false, namespace: 'default', comment: '', refreshInterval: '', response: '', v4: '', v6: '', enabled: true, policies: null };
    const s = wizardSources({
      ...base,
      curated: ['oisd-small'],
      urls: [
        { id: 1, url: 'https://small.oisd.nl/', format: 'adblock', formatTouched: false },
        { id: 2, url: 'https://small.oisd.nl/', format: 'adblock', formatTouched: false },
      ],
      paste: 'a.example',
    });
    expect(s.map((x) => x.name)).toEqual(['oisd-small', 'small', 'small-2', 'custom']);
  });

  it('adds a list reference once, bare in the same namespace', () => {
    const list = { apiVersion: 'kudurru.io/v1alpha1', kind: 'BlockList', metadata: { name: 'x', namespace: 'default' }, spec: { mode: 'allow', sources: [] } } as BlockList;
    const p = { apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name: 'p', namespace: 'other' }, spec: { allowListRefs: ['a'] } } as Policy;
    const once = withListRef(p, list);
    expect(once.spec.allowListRefs).toEqual(['a', 'default/x']);
    expect(withListRef(once, list)).toBe(once);
  });
});
