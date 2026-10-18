// Renders the whole app (providers, session gate, shell, routed page) at a
// route against the fetch stand-in of ui/src/dev/api.ts, signed in as a fixture
// person (or signed out with `me: null`). The session, the catalog, the
// open-request count and (empty) Order Form lists answer by default; tests
// add or override handlers.
import { QueryClient } from '@tanstack/react-query';
import { render, type RenderResult } from '@testing-library/react';
import { vi } from 'vitest';
import type { Catalog, Me } from '@shared/api';
import { AppProviders, Gate } from '../App';
import { apiError, createFetchMock, type FetchMock, type MockHandlers } from '../dev/api';
import { catalog as catalogFixture } from '../dev/fixtures';

export function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity, refetchInterval: false, refetchOnWindowFocus: false }, mutations: { retry: false } },
  });
}

export interface RenderAppOptions {
  /** The app route, e.g. "/licenses?new=1". */
  route: string;
  /** The signed-in person; null answers GET /auth/me with 401. */
  me: Me | null;
  handlers?: MockHandlers;
  catalog?: Catalog;
}

export function renderApp({ route, me, handlers = {}, catalog = catalogFixture() }: RenderAppOptions): RenderResult & { api: FetchMock; client: QueryClient } {
  const api = createFetchMock({
    authMe: () => (me ? me : apiError(401, 'unauthenticated', 'Sign in first')),
    catalog: () => catalog,
    requestsList: () => ({ items: [] }),
    myOrderForms: () => ({ items: [] }),
    orderFormsList: () => ({ items: [] }),
    ...handlers,
  });
  vi.stubGlobal('fetch', api.fetch);
  window.history.replaceState(null, '', route);
  const client = testQueryClient();
  const result = render(
    <AppProviders basePath="/" client={client} apiBase="/api/v1">
      <Gate />
    </AppProviders>,
  );
  return { ...result, api, client };
}
