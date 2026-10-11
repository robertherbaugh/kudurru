// A minimal History-API router. The dashboard has a dozen flat routes, so a
// 60 KB router library buys nothing; this one is prefix aware (basePath),
// keeps search params as the single source of filter state (every drill-down
// is a shareable URL, §10 principle 3) and plugs into Astryx's LinkProvider.
import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useSyncExternalStore,
  type AnchorHTMLAttributes,
  type MouseEvent,
  type ReactNode,
} from 'react';
import { joinBase, stripBase } from './basePath';

interface RouterState {
  basePath: string;
  navigate: (to: string, opts?: { replace?: boolean }) => void;
}

const RouterContext = createContext<RouterState>({
  basePath: '/',
  navigate: () => undefined,
});

const listeners = new Set<() => void>();
function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  window.addEventListener('popstate', fn);
  return () => {
    listeners.delete(fn);
    window.removeEventListener('popstate', fn);
  };
}
function notify(): void {
  for (const fn of listeners) fn();
}
function snapshot(): string {
  return window.location.pathname + window.location.search;
}

export function RouterProvider({ basePath, children }: { basePath: string; children: ReactNode }) {
  const navigate = useCallback(
    (to: string, opts?: { replace?: boolean }) => {
      const url = isAppRoute(to) ? joinBase(basePath, to) : to;
      if (opts?.replace) window.history.replaceState(null, '', url);
      else window.history.pushState(null, '', url);
      notify();
      if (!opts?.replace) window.scrollTo?.(0, 0);
    },
    [basePath],
  );
  const value = useMemo(() => ({ basePath, navigate }), [basePath, navigate]);
  return <RouterContext.Provider value={value}>{children}</RouterContext.Provider>;
}

/** App routes start with a single "/"; anything else is left alone. */
export function isAppRoute(to: string): boolean {
  return to.startsWith('/') && !to.startsWith('//') && !isApiURL(to);
}

/**
 * API URLs (exports, the OIDC start, downloads) come from ApiClient.url
 * and already carry the base path; they are full page loads, never routes.
 */
export function isApiURL(to: string): boolean {
  return /^\/(?:[^?#]*\/)?api\/v\d+(?:[/?#]|$)/.test(to);
}

export interface AppLocation {
  /** Route path without the base, starting with "/". */
  path: string;
  search: URLSearchParams;
  /** path + search, the form Link and navigate accept. */
  route: string;
}

export function useLocation(): AppLocation {
  const { basePath } = useContext(RouterContext);
  const raw = useSyncExternalStore(subscribe, snapshot, snapshot);
  return useMemo(() => {
    const q = raw.indexOf('?');
    const pathname = q >= 0 ? raw.slice(0, q) : raw;
    const search = q >= 0 ? raw.slice(q) : '';
    const path = stripBase(basePath, pathname);
    return { path, search: new URLSearchParams(search), route: path + search };
  }, [raw, basePath]);
}

export function useNavigate(): RouterState['navigate'] {
  return useContext(RouterContext).navigate;
}

export function useBasePath(): string {
  return useContext(RouterContext).basePath;
}

/**
 * Search params as state: the setter merges (null/"" deletes) and replaces
 * the history entry by default, so typing in a filter does not flood Back.
 */
export function useSearchParams(): [
  URLSearchParams,
  (patch: Record<string, string | number | boolean | null | undefined>, opts?: { push?: boolean }) => void,
] {
  const loc = useLocation();
  const navigate = useNavigate();
  const set = useCallback(
    (patch: Record<string, string | number | boolean | null | undefined>, opts?: { push?: boolean }) => {
      const next = new URLSearchParams(loc.search);
      for (const [k, v] of Object.entries(patch)) {
        if (v === null || v === undefined || v === '' || v === false) next.delete(k);
        else next.set(k, String(v));
      }
      const s = next.toString();
      navigate(loc.path + (s ? `?${s}` : ''), { replace: !opts?.push });
    },
    [loc, navigate],
  );
  return [loc.search, set];
}

/** Matches "/zones/:zone" against a path; returns params or null. */
export function matchPath(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split('/').filter(Boolean);
  const s = path.split('/').filter(Boolean);
  if (p.length !== s.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    const seg = p[i] as string;
    const val = s[i] as string;
    if (seg.startsWith(':')) {
      params[seg.slice(1)] = decodeURIComponent(val);
    } else if (seg !== val) {
      return null;
    }
  }
  return params;
}

export type LinkProps = Omit<AnchorHTMLAttributes<HTMLAnchorElement>, 'download'> & {
  href?: string;
  /** Download attribute (React types it as any). */
  download?: string | boolean;
  /** Astryx passes `to` alongside href for custom link components. */
  to?: string;
  replace?: boolean;
};

/**
 * An anchor that routes app paths ("/queries?...") client side and leaves
 * absolute URLs, downloads and new-tab clicks to the browser.
 */
export function Link({ href, to, replace, onClick, target, download, ...rest }: LinkProps) {
  const { basePath, navigate } = useContext(RouterContext);
  const dest = to ?? href ?? '#';
  const app = isAppRoute(dest);
  const resolved = app ? joinBase(basePath, dest) : dest;
  const handle = (e: MouseEvent<HTMLAnchorElement>) => {
    onClick?.(e);
    if (
      e.defaultPrevented ||
      !app ||
      e.button !== 0 ||
      e.metaKey ||
      e.ctrlKey ||
      e.shiftKey ||
      e.altKey ||
      (target && target !== '_self') ||
      download !== undefined
    ) {
      return;
    }
    e.preventDefault();
    navigate(dest, replace ? { replace } : undefined);
  };
  return <a {...rest} href={resolved} target={target} download={download} onClick={handle} />;
}
