import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { Summary } from '../api/types';
import { renderWithApp } from '../test/utils';
import { StatTiles, tilesFor } from './StatTiles';

const summary: Summary = {
  scope: 'cluster',
  nodes: [{ name: 'kudurru-0', ok: true, took_ms: 1 }],
  partial: false,
  from: '2026-10-09T18:00:00Z',
  until: '2026-10-10T18:00:00Z',
  queries: {
    total: 128_431,
    blocked: 21_833,
    percent_blocked: 17.0,
    cached: 51_000,
    forwarded: 50_000,
    authoritative: 5_000,
    dropped: 0,
    other: 598,
    unique_domains: 4_312,
    qps_1m: 2.4,
    qps_avg: 1.5,
  },
  clients: { active: 9, total: 12 },
  domains_on_lists: 136_100,
  rcodes: {},
  protocols: {},
  types: {},
  latency: { avg_ms: 1, p50_ms: 1, p95_ms: 2, p99_ms: 3 },
  approximate: false,
};

describe('tilesFor', () => {
  it.each([
    ['total', '128.4K', '/queries?since=2026-10-09T18%3A00%3A00Z'],
    ['blocked', '21.8K', '/queries?status_class=blocked&since=2026-10-09T18%3A00%3A00Z'],
    ['percent', '17.0 %', '/queries?status_class=blocked&since=2026-10-09T18%3A00%3A00Z'],
    ['lists', '136.1K', '/lists'],
  ])('%s tile shows %s and links to %s', (key, value, href) => {
    const t = tilesFor(summary).find((x) => x.key === key);
    expect(t?.value).toBe(value);
    expect(t?.href).toBe(href);
  });

  it('puts active/total clients in the footer', () => {
    expect(tilesFor(summary)[0]?.foot).toBe('9 active of 12 clients');
  });

  it('renders placeholders without a summary', () => {
    expect(tilesFor(undefined).map((t) => t.value)).toEqual(['–', '–', '–', '–']);
  });
});

describe('StatTiles', () => {
  it('renders four links into the pre-filtered log', () => {
    renderWithApp(<StatTiles summary={summary} />);
    const list = screen.getByRole('list', { name: 'Summary' });
    const links = within(list).getAllByRole('listitem');
    expect(links).toHaveLength(4);
    expect(screen.getByTestId('tile-total')).toHaveTextContent('128.4K');
    expect(screen.getByTestId('tile-total')).toHaveAttribute('title', '128,431');
    const blocked = screen.getByRole('listitem', { name: /Queries blocked: 21,833/ });
    expect(blocked).toHaveAttribute('href', '/queries?status_class=blocked&since=2026-10-09T18%3A00%3A00Z');
    expect(screen.getByRole('listitem', { name: /Domains on lists/ })).toHaveAttribute('href', '/lists');
  });

  it('shows an ellipsis while loading', () => {
    renderWithApp(<StatTiles summary={undefined} />);
    expect(screen.getByTestId('tile-blocked')).toHaveTextContent('…');
  });
});
