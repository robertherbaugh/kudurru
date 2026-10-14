// The application root, after web/src/App.tsx: the providers (query cache,
// API client, router, appearance, Astryx links and layers for toasts) and
// the session gate. Anonymous pages (/login, /verify, /approve) render
// without asking for a session at all; every other page needs one, and a
// visitor without it goes to /login?next=<the page>, so an invite link
// survives the sign-in round trip.
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Center } from '@astryxdesign/core/Center';
import { LayerProvider } from '@astryxdesign/core/Layer';
import { LinkProvider } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { Suspense, useEffect, useState, type ReactNode } from 'react';
import { ApiError } from './api/client';
import { ApiProvider, SESSION_KEY, useSession } from './api/context';
import { Shell } from './components/Shell';
import { resolveRoute } from './routes';
import { ErrorBoundary } from './ui/components/ErrorBoundary';
import { errorText, FullPageSpinner } from './ui/components/Feedback';
import { apiBaseFor } from './ui/lib/basePath';
import { Link, RouterProvider, useLocation, useNavigate } from './ui/lib/router';
import { AppearanceProvider } from './ui/lib/theme';

/** A QueryClient with defaults suited to a back office talking to one API. */
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
      <ApiProvider baseUrl={apiBase ?? apiBaseFor(basePath)} onUnauthenticated={() => client.setQueryData(SESSION_KEY, null)}>
        <RouterProvider basePath={basePath}>
          <AppearanceProvider>
            <LinkProvider component={Link}>
              <LayerProvider>{children}</LayerProvider>
            </LinkProvider>
          </AppearanceProvider>
        </RouterProvider>
      </ApiProvider>
    </QueryClientProvider>
  );
}

/** The sign-in page's address for coming back to `route`. */
export function loginRoute(route: string): string {
  return route === '/' || route.startsWith('/login') ? '/login' : `/login?next=${encodeURIComponent(route)}`;
}

function RedirectToLogin() {
  const loc = useLocation();
  const navigate = useNavigate();
  useEffect(() => {
    navigate(loginRoute(loc.route), { replace: true });
  }, [loc.route, navigate]);
  return <FullPageSpinner label="Opening the sign-in page" />;
}

function SessionGate() {
  const session = useSession();
  if (session.isPending) return <FullPageSpinner label="Connecting to the license portal" />;
  // Only a first check that failed blocks the app. A failed refetch (a
  // network blip, a focus refetch after sleep, a deploy) keeps the session
  // it had (TanStack Query keeps `data` with isError), so open wizards and
  // forms survive; the shell says the check failed (ShellBanners).
  if (session.isError && session.data === undefined) {
    return (
      <Center minHeight="100dvh" padding={4} role="main">
        <VStack gap={3} maxWidth={520}>
          <Banner status="error" title="The license portal could not be reached" description={errorText(session.error)} />
          <Button label="Try again" variant="secondary" onClick={() => void session.refetch()} />
        </VStack>
      </Center>
    );
  }
  if (!session.data) return <RedirectToLogin />;
  return <Shell />;
}

/** Anonymous pages: no shell, no session query. */
function PublicPage() {
  const loc = useLocation();
  const match = resolveRoute(loc.path);
  if (!match) return null;
  const Page = match.route.page;
  return (
    <ErrorBoundary key={loc.path}>
      <Suspense fallback={<FullPageSpinner label="Loading page" />}>
        <Page params={match.params} />
      </Suspense>
    </ErrorBoundary>
  );
}

export function Gate() {
  const loc = useLocation();
  const match = resolveRoute(loc.path);
  if (match?.route.access === 'anonymous') return <PublicPage />;
  return <SessionGate />;
}

export function App({ basePath, apiBase }: { basePath: string; apiBase?: string }) {
  const [client] = useState(makeQueryClient);
  return (
    <AppProviders basePath={basePath} client={client} {...(apiBase ? { apiBase } : {})}>
      <Gate />
    </AppProviders>
  );
}
