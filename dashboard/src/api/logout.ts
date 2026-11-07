/** Accept only the API's same-origin provider continuation. Token hints never
 * cross this boundary; they stay in encrypted HttpOnly cookies. */
export function browserLogoutContinuation(baseUrl: string, path: string | undefined, origin: string): string | undefined {
  if (typeof path !== 'string' || !path.startsWith('/') || path.startsWith('//') || path.includes('\\') || path.includes('%')) return undefined;
  const base = new URL(baseUrl, origin);
  const target = new URL(path, origin);
  const prefix = `${base.pathname.replace(/\/+$/, '')}/auth/oidc/`;
  if (base.origin !== origin || target.origin !== origin || target.search || target.hash || target.pathname !== path || !target.pathname.startsWith(prefix)) return undefined;
  const provider = target.pathname.slice(prefix.length);
  return /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\/logout$/.test(provider) ? target.pathname : undefined;
}
