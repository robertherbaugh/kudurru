// The application root: providers (query cache, API client, appearance,
// router, node scope, toasts) and the session gate. Signed-out visitors get
// the login page; everyone else gets the shell with the routed pages.
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { LinkProvider } from '@astryxdesign/core/Link';
import { LayerProvider } from '@astryxdesign/core/Layer';
import { useState, type ReactNode } from 'react';
import { ApiError } from './api/client';
import { ApiProvider, qk, ScopeProvider, useSession } from './api/context';
import { apiBaseFor } from './lib/basePath';
import { Link, RouterProvider } from './lib/router';
import { AppearanceProvider } from './lib/theme';
import { Shell } from './components/Shell';
import { LoginPage } from './pages/LoginPage';
import { FullPageSpinner } from './components/Feedback';

/** A QueryClient with defaults suited to a dashboard polling one API. */
export function makeQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        staleTime: 5_000,
        refetchOnWindowFocus: true,
        retry: (count, err) => {
          if (err instanceof ApiError && err.status >= 400 && err.status < 500) return false;
          return count < 2;
        },
      },
      mutations: { retry: false },
    },
  });
}

/** Providers shared by the app and the component tests. */
export function AppProviders({
  basePath,
  client,
  apiBase,
  children,
}: {
  basePath: string;
  client: QueryClient;
  /** Overrides the API base derived from basePath (tests use an absolute URL). */
  apiBase?: string;
  children: ReactNode;
}) {
  return (
    <QueryClientProvider client={client}>
      <ApiProvider baseUrl={apiBase ?? apiBaseFor(basePath)} onUnauthenticated={() => client.setQueryData(qk.me, null)}>
        <RouterProvider basePath={basePath}>
          <AppearanceProvider>
            <LinkProvider component={Link}>
              <LayerProvider>
                <ScopeProvider>{children}</ScopeProvider>
              </LayerProvider>
            </LinkProvider>
          </AppearanceProvider>
        </RouterProvider>
      </ApiProvider>
    </QueryClientProvider>
  );
}

function Gate() {
  const session = useSession();
  if (session.isPending) return <FullPageSpinner label="Connecting to kudurru" />;
  if (!session.data) return <LoginPage />;
  return <Shell />;
}

export function App({ basePath, apiBase }: { basePath: string; apiBase?: string }) {
  const [client] = useState(makeQueryClient);
  return (
    <AppProviders basePath={basePath} client={client} {...(apiBase ? { apiBase } : {})}>
      <Gate />
    </AppProviders>
  );
}
