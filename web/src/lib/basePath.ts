// Path-prefix awareness (§10): the Go handler rewrites `<base href>` in
// index.html to the prefix the dashboard is served under (for example
// "/kudurru/" behind a reverse proxy). Everything else derives from it: the
// router's basename, the API base and links to API downloads.

/** The base path with a leading and trailing slash, e.g. "/" or "/kudurru/". */
export function detectBasePath(doc: Document = document): string {
  let path: string;
  try {
    path = new URL(doc.baseURI).pathname;
  } catch {
    path = '/';
  }
  if (!path.startsWith('/')) path = `/${path}`;
  // A base of "/kudurru/index.html" still means the "/kudurru/" directory.
  path = path.replace(/[^/]*$/, '');
  return path || '/';
}

/** The API base for a base path: "/kudurru/" → "/kudurru/api/v1". */
export function apiBaseFor(basePath: string): string {
  return `${basePath.replace(/\/+$/, '')}/api/v1`;
}

/**
 * Joins an app route ("/queries?x=1") onto the base path
 * ("/kudurru/" → "/kudurru/queries?x=1").
 */
export function joinBase(basePath: string, route: string): string {
  const b = basePath.endsWith('/') ? basePath : `${basePath}/`;
  return b + route.replace(/^\/+/, '');
}

/** Strips the base path from a pathname; the result starts with "/". */
export function stripBase(basePath: string, pathname: string): string {
  const b = basePath.endsWith('/') ? basePath : `${basePath}/`;
  if (pathname === b.slice(0, -1)) return '/';
  if (pathname.startsWith(b)) return `/${pathname.slice(b.length)}`;
  return pathname || '/';
}
