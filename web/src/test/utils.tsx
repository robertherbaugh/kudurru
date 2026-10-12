// Test helpers: render inside the real providers against the MSW handlers
// (src/mocks), signed in or not, at a given route.
import { QueryClient } from '@tanstack/react-query';
import { render, type RenderResult } from '@testing-library/react';
import type { ReactElement } from 'react';
import type { SettingKey } from '../api/types';
import { AppProviders } from '../App';
import { mockDB, signIn } from '../mocks/handlers';

/** The values of the structured entries of a mock list (the ui-allow / ui-deny lists). */
export function uiEntryValues(listName: string, namespace = 'default'): string[] {
  const l = mockDB().lists.find((x) => x.metadata.name === listName && (x.metadata.namespace ?? 'default') === namespace);
  return (l?.spec.sources ?? []).flatMap((s) => (s.entries ?? []).map((e) => e.value));
}

/** The absolute API base the tests use (Node's fetch needs absolute URLs). */
export const API = `${window.location.origin}/api/v1`;

export function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity, refetchInterval: false, refetchOnWindowFocus: false }, mutations: { retry: false } },
  });
}

export interface RenderOptions {
  /** App route, e.g. "/queries?status_class=blocked". */
  route?: string;
  /** Sign in as this mock user first (default admin); null stays signed out. */
  user?: string | null;
  client?: QueryClient;
}

/** Renders ui inside AppProviders; returns the RTL result and the query client. */
export function renderWithApp(ui: ReactElement, opts: RenderOptions = {}): RenderResult & { client: QueryClient } {
  window.history.replaceState(null, '', opts.route ?? '/');
  if (opts.user !== null) signIn(opts.user ?? 'admin');
  const client = opts.client ?? testQueryClient();
  const result = render(
    <AppProviders basePath="/" client={client} apiBase={API}>
      {ui}
    </AppProviders>,
  );
  return { ...result, client };
}

/** A settings schema row with the flags the server always sends (all false by default). */
export function settingKey(k: Pick<SettingKey, 'path' | 'type' | 'scope'> & Partial<SettingKey>): SettingKey {
  return { default: '', help: '', hostOnly: false, envLocked: false, restartRequired: false, writeOnly: false, ...k };
}
