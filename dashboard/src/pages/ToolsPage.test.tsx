import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { splitPresentation } from '../lib/rr';
import { findSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';
import ToolsPage from './ToolsPage';

/** Records the bodies of POST /dns/query while passing through to the mock. */
function spyDig(): Record<string, unknown>[] {
  const seen: Record<string, unknown>[] = [];
  server.events.on('request:start', ({ request }) => {
    if (new URL(request.url).pathname === '/api/v1/dns/query') void request.clone().json().then((b) => seen.push(b as Record<string, unknown>));
  });
  return seen;
}

async function dig(name: string) {
  const panel = await screen.findByRole('region', { name: 'Query (dig)' });
  fireEvent.change(within(panel).getByRole('textbox', { name: /^Name/ }), { target: { value: name } });
  fireEvent.click(within(panel).getByRole('button', { name: 'Query' }));
  return screen.findByTestId('dig-result');
}

describe('splitPresentation', () => {
  it('splits miekg presentation lines into columns and leaves the rest alone', () => {
    expect(splitPresentation('example.com.\t153\tIN\tA\t172.66.147.243')).toEqual({ name: 'example.com.', ttl: '153', class: 'IN', type: 'A', data: '172.66.147.243' });
    expect(splitPresentation('x.\t60\tIN\tSOA\tns. host. 1 2 3 4 5')?.data).toBe('ns. host. 1 2 3 4 5');
    expect(splitPresentation('\n;; OPT PSEUDOSECTION:\n; EDNS: version 0')).toBeNull();
  });
});

describe('ToolsPage dig', () => {
  it('renders the string answer lines, the pipeline latency and the flags', async () => {
    renderWithApp(<ToolsPage />, { route: '/tools' });
    const r = await dig('www.youtube.com');
    const answer = within(r).getByRole('table', { name: 'answer section' });
    expect(within(answer).getAllByText('youtube-ui.l.google.com.').length).toBe(2);
    expect(within(answer).getByText('104.16.132.229')).toBeInTheDocument();
    expect(within(answer).getAllByText('IN').length).toBe(2);
    expect(within(r).getByText(/in the pipeline/)).toHaveTextContent('18.6 ms in the pipeline');
    expect(within(r).getByText('flags: rd ra')).toBeInTheDocument();
    expect(within(r).getByText('default/cloudflare')).toBeInTheDocument();
    expect(r.textContent).not.toMatch(/undefined|NaN/);
    // The e2e specs take the panel's last polite live region as the result.
    const panel = screen.getByRole('region', { name: 'Query (dig)' });
    expect([...panel.querySelectorAll('[aria-live="polite"]')].at(-1)).toBe(r);
  });

  it('sends the transport and the DO bit, and shows a pipeline error', async () => {
    const seen = spyDig();
    renderWithApp(<ToolsPage />, { route: '/tools' });
    pick(await findSelector('Transport'), 'DoT');
    fireEvent.click(screen.getByRole('checkbox', { name: 'DNSSEC (DO bit)' }));
    const r = await dig('example.org');
    await waitFor(() => expect(seen[0]).toMatchObject({ name: 'example.org', type: 'A', proto: 'dot', dnssec: true }));
    expect(within(r).getByText('flags: rd ra ad')).toBeInTheDocument();
    expect(within(r).getByText('RRSIG')).toBeInTheDocument();

    const bad = await dig('servfail.example');
    expect(await within(bad).findByText('The pipeline reported an error')).toBeInTheDocument();
    expect(within(bad).getByText('every upstream of group default failed: i/o timeout')).toBeInTheDocument();
    expect(within(bad).getByText('SERVFAIL')).toBeInTheDocument();
  });
});
