import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, optionLabels, pick, valueOf } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import ListDetailPage, { policiesUsing } from './ListDetailPage';

function renderList(name: string, ns = 'default') {
  return renderWithApp(<ListDetailPage params={{ ns, name }} />, { route: `/lists/${ns}/${name}` });
}

const family = () => mockDB().lists.find((l) => l.metadata.name === 'family');
const starter = () => mockDB().lists.find((l) => l.metadata.name === 'starter');

/** The enable switch of a source row. */
async function sourceSwitch(name: string): Promise<HTMLElement> {
  const row = await screen.findByTestId(`source-${name}`);
  const sw = within(row).getByRole('switch', { name: `Source ${name} enabled` });
  await waitFor(() => expect(sw).not.toBeDisabled());
  return sw;
}

describe('ListDetailPage', () => {
  it('shows the current state: entries, sources, unsupported rules and the policies using it', async () => {
    renderList('family');
    expect(await screen.findByRole('heading', { level: 1, name: 'family' })).toBeInTheDocument();
    expect(screen.getByText('Kids and guests')).toBeInTheDocument();
    expect(screen.getByText('312,407')).toBeInTheDocument();
    expect(screen.getByText('3 of 4 enabled')).toBeInTheDocument();
    expect(screen.getByTestId('unsupported')).toHaveTextContent('214 parsed, not applied');
    expect(await screen.findByRole('link', { name: 'kids-bedtime' })).toHaveAttribute('href', '/policies?q=kids-bedtime');
    // One row per source: type, location, format, subdomains, per-source status.
    const nsfw = screen.getByTestId('source-oisd-nsfw');
    expect(within(nsfw).getByText('https://nsfw.oisd.nl/')).toBeInTheDocument();
    expect(within(nsfw).getByText('yes')).toBeInTheDocument();
    expect(within(nsfw).getByText('144,184')).toBeInTheDocument();
    const gambling = screen.getByTestId('source-gambling');
    expect(within(gambling).getByRole('switch', { name: 'Source gambling enabled' })).not.toBeChecked();
    expect(within(gambling).getByText('disabled')).toBeInTheDocument();
    expect(within(screen.getByTestId('source-extra')).getByRole('button', { name: /Browse the 3 inline entries/ })).toBeInTheDocument();
  });

  it('turns a source off at once and saves spec.sources[].enabled', async () => {
    renderList('family');
    const sw = await sourceSwitch('hagezi-pro');
    expect(sw).toBeChecked();
    fireEvent.click(sw);
    await waitFor(() => expect(family()?.spec.sources[0]?.enabled).toBe(false));
    expect(sw).not.toBeChecked();
    expect(await screen.findByText('hagezi-pro disabled')).toBeInTheDocument();
    expect(await screen.findByText('2 of 4 enabled')).toBeInTheDocument();
    // Turning one back on omits the field (the default).
    fireEvent.click(await sourceSwitch('gambling'));
    await waitFor(() => expect(family()?.spec.sources[2]).not.toHaveProperty('enabled'));
  });

  it('moves the switch at once and back when the write fails', async () => {
    let release = () => {};
    const gate = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.put(`${API}/objects/blocklists/default/family`, async () => {
        await gate;
        return HttpResponse.json({ error: { key: 'invalid', message: 'spec.sources[0].enabled: unknown field' } }, { status: 400 });
      }),
    );
    renderList('family');
    const sw = await sourceSwitch('hagezi-pro');
    fireEvent.click(sw);
    // Optimistic: off while the server has not answered yet.
    await waitFor(() => expect(sw).not.toBeChecked());
    release();
    expect(await screen.findByText(/Could not disable hagezi-pro: .*unknown field/)).toBeInTheDocument();
    await waitFor(() => expect(sw).toBeChecked());
    expect(family()?.spec.sources[0]).not.toHaveProperty('enabled');
  });

  it('adds a URL source from the dialog with the format picked from the URL', async () => {
    renderList('family');
    fireEvent.click(await screen.findByRole('button', { name: 'Add source' }));
    const url = await screen.findByRole('textbox', { name: /^URL/ });
    fireEvent.change(url, { target: { value: 'https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/tif.txt' } });
    expect(valueOf(getSelector(/^Format/))).toBe('adblock');
    expect(screen.getByRole('textbox', { name: /^Name/ })).toHaveValue('hagezi-tif');
    fireEvent.click(screen.getByRole('switch', { name: /Include subdomains/ }));
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Add source' }));
    await waitFor(() => expect(family()?.spec.sources).toHaveLength(5));
    expect(family()?.spec.sources[4]).toEqual({ name: 'hagezi-tif', url: 'https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/tif.txt', format: 'adblock', subdomains: true });
  });

  it('adds pasted entries as an inline source and validates them first', async () => {
    renderList('family');
    fireEvent.click(await screen.findByRole('button', { name: 'Add source' }));
    fireEvent.click(await screen.findByRole('radio', { name: 'Paste entries' }));
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'extra' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^Entries/ }), { target: { value: 'good.example\nbad..example' } });
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Add source' }));
    expect(await screen.findByText('Another source of this list has this name')).toBeInTheDocument();
    expect(screen.getByText(/1 invalid, first "bad..example"/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'pasted' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^Entries/ }), { target: { value: '# mine\ngood.example\nother.example' } });
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Add source' }));
    await waitFor(() => expect(family()?.spec.sources[4]).toEqual({ name: 'pasted', format: 'auto', inline: ['good.example', 'other.example'] }));
  });

  it('edits a source in a small dialog and deletes one after confirmation', async () => {
    renderList('family');
    fireEvent.click(within(await screen.findByTestId('source-oisd-nsfw')).getByRole('button', { name: 'Edit' }));
    const format = await findSelector(/^Format/, await screen.findByRole('dialog'));
    expect(valueOf(format)).toBe('adblock');
    pick(format, 'One domain per line');
    fireEvent.click(screen.getByRole('button', { name: 'Save source' }));
    await waitFor(() => expect(family()?.spec.sources[1]?.format).toBe('domains'));
    expect(family()?.spec.sources[1]?.subdomains).toBe(true);

    fireEvent.click(within(screen.getByTestId('source-gambling')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Delete source' }));
    await waitFor(() => expect(family()?.spec.sources.map((s) => s.name)).toEqual(['hagezi-pro', 'oisd-nsfw', 'extra']));
  });

  it('browses, searches, adds and removes inline entries without a text box of every entry', async () => {
    renderList('starter');
    fireEvent.click(await screen.findByRole('button', { name: /Browse the 2,852 inline entries of embedded/ }));
    const browser = await screen.findByTestId('entries-browser');
    expect(await within(browser).findByText('Showing 1–25 of 2,852')).toBeInTheDocument();
    expect(window.location.search).toBe('?source=embedded');
    fireEvent.click(within(browser).getByRole('button', { name: 'Next page' }));
    expect(await within(browser).findByText('Showing 26–50 of 2,852')).toBeInTheDocument();

    fireEvent.change(within(browser).getByRole('textbox', { name: /^Search entries/ }), { target: { value: 'tiktokv' } });
    expect(await within(browser).findByText('tiktokv.com')).toBeInTheDocument();
    await waitFor(() => expect(within(browser).getAllByRole('row')).toHaveLength(2));

    fireEvent.change(within(browser).getByRole('textbox', { name: /^Add entries/ }), { target: { value: 'new-tracker.example\nsecond.example' } });
    fireEvent.click(within(browser).getByRole('button', { name: 'Add 2' }));
    await waitFor(() => expect(starter()?.spec.sources[0]?.inline).toContain('new-tracker.example'));
    expect(starter()?.spec.sources[0]?.inline).toHaveLength(2854);

    fireEvent.click(within(browser).getByRole('checkbox', { name: 'Select tiktokv.com' }));
    fireEvent.click(within(browser).getByRole('button', { name: 'Remove 1 selected' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(starter()?.spec.sources[0]?.inline).not.toContain('tiktokv.com'));
    expect(await screen.findByText('Removed 1 entry from embedded')).toBeInTheDocument();
  });

  it('shows a file-managed list read-only with its owner', async () => {
    renderList('regex-trackers');
    expect(await screen.findByText('Read-only: managed by file:objects.d/lists.yaml')).toBeInTheDocument();
    expect(screen.getByText('Managed by objects.d/lists.yaml; edit the file on the host.')).toBeInTheDocument();
    expect(within(screen.getByTestId('source-patterns')).getByRole('switch')).toBeDisabled();
    expect(screen.getByRole('switch', { name: 'Enabled' })).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Add source' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Save settings' })).toBeNull();
    expect(within(screen.getByTestId('source-patterns')).queryByRole('button', { name: 'Edit' })).toBeNull();
    // The entries can still be read.
    fireEvent.click(within(screen.getByTestId('source-patterns')).getByRole('button', { name: 'Entries' }));
    const browser = await screen.findByTestId('entries-browser');
    expect(await within(browser).findByText('/^telemetry\\./')).toBeInTheDocument();
    expect(within(browser).queryByRole('textbox', { name: /^Add entries/ })).toBeNull();
  });

  it('saves the list settings with typed fields', async () => {
    renderList('family');
    const response = await findSelector(/^Block response/);
    expect(valueOf(response)).toBe('nxdomain');
    pick(response, 'Custom IP');
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText('Give an IPv4 or IPv6 address')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Custom IPv4/ }), { target: { value: '192.0.2.80' } });
    pick(getSelector(/^Refresh interval/), 'Every week');
    fireEvent.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect(family()?.spec.response).toBe('customIP'));
    expect(family()?.spec.customIP).toEqual({ v4: '192.0.2.80' });
    expect(family()?.spec.refreshInterval).toBe('168h');
    expect(family()?.metadata.annotations?.['kudurru.io/comment']).toBe('Kids and guests');
  });

  it('offers the block page response and shows the list as YAML', async () => {
    renderList('family');
    const response = await findSelector(/^Block response/);
    expect(optionLabels(response)).toContain('Block page');
    fireEvent.click(screen.getByRole('button', { name: 'View as YAML' }));
    const yaml = await screen.findByLabelText('YAML');
    expect(yaml).toHaveTextContent('kind: BlockList');
    expect(yaml).toHaveTextContent('enabled: false');
    expect(yaml.textContent).not.toContain('status');
    expect(yaml.textContent).not.toContain('resourceVersion');
  });

  it('turns the whole list off from the header', async () => {
    renderList('family');
    const sw = await screen.findByRole('switch', { name: 'Enabled' });
    await waitFor(() => expect(sw).not.toBeDisabled());
    fireEvent.click(sw);
    await waitFor(() => expect(family()?.metadata.annotations?.['kudurru.io/enabled']).toBe('false'));
    expect(await screen.findByText('This list is disabled')).toBeInTheDocument();
  });

  it('finds the policies that reference a list', () => {
    const p = (name: string, ns: string, block: string[], allow: string[] = []) =>
      ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name, namespace: ns }, spec: { blockListRefs: block, allowListRefs: allow } }) as const;
    const used = policiesUsing([p('a', 'default', ['family']), p('b', 'other', ['family']), p('c', 'other', ['default/family']), p('d', 'default', [], ['family'])], 'default', 'family');
    expect(used.map((u) => `${u.policy.metadata.name}:${u.as}`)).toEqual(['a:block', 'c:block', 'd:allow']);
  });
});

describe('EntriesBrowser on every kind of source', () => {
  const steven = () => mockDB().lists.find((l) => l.metadata.name === 'stevenblack');

  async function openBrowser(list: string, source: string): Promise<HTMLElement> {
    renderList(list);
    fireEvent.click(within(await screen.findByTestId(`source-${source}`)).getByRole('button', { name: 'Entries' }));
    return screen.findByTestId('entries-browser');
  }

  it('counts the enabled entries of a source in the sources table', async () => {
    renderList('stevenblack');
    expect(within(await screen.findByTestId('source-unified')).getByText('81,234 of 81,236 enabled')).toBeInTheDocument();
  });

  it('browses the fetched rules of a URL source and switches one back on', async () => {
    const browser = await openBrowser('stevenblack', 'unified');
    expect(await within(browser).findByText(/81,236 rules, 2 switched off/)).toBeInTheDocument();
    expect(within(browser).getByText(/affects only this list/)).toBeInTheDocument();
    expect(within(browser).getByRole('link', { name: 'Allowlist' })).toHaveAttribute('href', '/allowlist');
    expect(within(browser).getByRole('link', { name: 'Allowlist as list exceptions' })).toHaveAttribute('href', '/allowlist?kind=exception&scope=list%3Adefault%2Fstevenblack');
    // URL sources take no add or remove.
    expect(within(browser).queryByRole('textbox', { name: /^Add entries/ })).toBeNull();
    expect(within(browser).getByText('1 switched-off entry is no longer in the source')).toBeInTheDocument();

    fireEvent.click(within(browser).getByRole('switch', { name: 'Show disabled only' }));
    expect(await within(browser).findByText('Showing 1–2 of 2 switched off')).toBeInTheDocument();
    const sw = within(within(browser).getByTestId('list-entry-ads.unified.example')).getByRole('switch', { name: 'ads.unified.example enabled' });
    await waitFor(() => expect(sw).not.toBeDisabled());
    expect(sw).not.toBeChecked();
    fireEvent.click(sw);
    await waitFor(() => expect(steven()?.spec.sources[0]?.exclude).not.toContain('ads.unified.example'));
    expect(await screen.findByText('ads.unified.example switched on in stevenblack')).toBeInTheDocument();
  });

  it('switches a rule off at once and back when the write fails', async () => {
    let release = () => {};
    server.use(
      http.patch(`${API}/blocklists/default/stevenblack/sources/unified/entries`, async () => {
        await new Promise<void>((r) => (release = r));
        return HttpResponse.json({ error: { key: 'conflict', message: 'the list changed meanwhile' } }, { status: 409 });
      }),
    );
    const browser = await openBrowser('stevenblack', 'unified');
    fireEvent.change(within(browser).getByRole('textbox', { name: /^Search entries/ }), { target: { value: 'pixel.unified' } });
    const r = await within(browser).findByTestId('list-entry-pixel.unified.example');
    const sw = within(r).getByRole('switch');
    await waitFor(() => expect(sw).not.toBeDisabled());
    expect(sw).toBeChecked();
    fireEvent.click(sw);
    await waitFor(() => expect(within(within(browser).getByTestId('list-entry-pixel.unified.example')).getByRole('switch')).not.toBeChecked());
    release();
    expect(await screen.findByText('Could not switch the entries off: the list changed meanwhile')).toBeInTheDocument();
    await waitFor(() => expect(within(within(browser).getByTestId('list-entry-pixel.unified.example')).getByRole('switch')).toBeChecked());
  });

  it('switches several rules off at once and forgets the stale ones', async () => {
    const browser = await openBrowser('stevenblack', 'unified');
    fireEvent.change(within(browser).getByRole('textbox', { name: /^Search entries/ }), { target: { value: 'beacon' } });
    await within(browser).findByTestId('list-entry-beacon.unified.example');
    fireEvent.click(within(browser).getByRole('checkbox', { name: 'Select beacon.unified.example' }));
    fireEvent.click(within(browser).getByRole('checkbox', { name: 'Select beacon1.unified.example' }));
    fireEvent.click(within(within(browser).getByRole('toolbar', { name: 'Bulk actions' })).getByRole('button', { name: 'Switch off' }));
    await waitFor(() => expect(steven()?.spec.sources[0]?.exclude).toEqual(expect.arrayContaining(['beacon.unified.example', 'beacon1.unified.example'])));
    expect(await screen.findByText('Switched off 2 entries in stevenblack')).toBeInTheDocument();

    fireEvent.click(within(browser).getByRole('button', { name: 'Forget them' }));
    await waitFor(() => expect(steven()?.spec.sources[0]?.exclude).not.toContain('gone.tracker.example'));
  });

  it('switches structured entries of a ui list and points to the Allowlist', async () => {
    const browser = await openBrowser('ui-allow', 'ui');
    expect(await within(browser).findByText(/5 allow entries, 1 switched off/)).toBeInTheDocument();
    expect(within(browser).getByRole('link', { name: 'Allowlist' })).toHaveAttribute('href', '/allowlist');
    expect(within(browser).queryByRole('textbox', { name: /^Add entries/ })).toBeNull();
    const sw = within(within(browser).getByTestId('list-entry-s.youtube.com')).getByRole('switch');
    await waitFor(() => expect(sw).not.toBeDisabled());
    fireEvent.click(sw);
    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'ui-allow')?.spec.sources[0]?.entries?.[0]?.enabled).toBe(false));
  });

  it('switches inline lines off without removing them', async () => {
    const browser = await openBrowser('family', 'extra');
    const sw = within(await within(browser).findByTestId('list-entry-roblox.com')).getByRole('switch');
    await waitFor(() => expect(sw).not.toBeDisabled());
    fireEvent.click(sw);
    await waitFor(() => expect(family()?.spec.sources[3]?.exclude).toEqual(['roblox.com']));
    expect(family()?.spec.sources[3]?.inline).toContain('roblox.com');
  });

  it('says so when this node has not fetched the source yet', async () => {
    server.use(
      http.get(`${API}/blocklists/default/stevenblack/sources/unified/entries`, () =>
        HttpResponse.json({ error: { key: 'unavailable', message: 'the fetched body of source "unified" is not on this node yet', hint: 'try again after the next refresh' } }, { status: 503 }),
      ),
    );
    const browser = await openBrowser('stevenblack', 'unified');
    expect(await within(browser).findByText('This node has not fetched the list yet')).toBeInTheDocument();
    expect(within(browser).getByText(/try again after the next refresh/)).toBeInTheDocument();
  });
});
