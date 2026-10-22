// Format checks for free-text values (C1 `format`): the values that stay
// text (addresses, names, URLs) are validated inline with an example, the
// way internal/config checks them; the server stays the authority.
import type { SettingFormat } from '../../api/types';
import { hostError, isIPv4, isIPv6 } from '../../lib/rr';
import { hostPortError, serverError } from '../../lib/upstreams';

/** An example per format, used as the placeholder. */
export const FORMAT_EXAMPLES: Record<SettingFormat, string> = {
  ip: '192.0.2.10 or 2001:db8::10',
  ipv4: '192.0.2.10',
  ipv6: '2001:db8::10',
  cidr: '192.168.1.0/24',
  hostport: '0.0.0.0:80 or :80',
  url: 'https://example.com/path',
  domain: 'example.com',
  upstream: 'tls://dns.quad9.net',
  email: 'admin@example.com',
  'date-time': '2026-10-15T12:00:00Z',
};

function cidrError(v: string): string | null {
  const [addr = '', len, extra] = v.split('/');
  if (extra !== undefined) return 'One "/" at most';
  const v4 = isIPv4(addr);
  if (!v4 && !isIPv6(addr)) return 'Not an IP address or CIDR prefix';
  if (len === undefined) return null;
  const max = v4 ? 32 : 128;
  return /^\d{1,3}$/.test(len) && Number(len) <= max ? null : `The prefix length must be 0-${max}`;
}

/** The problem with a value of a format, or null; empty values are the caller's call. */
export function formatError(format: SettingFormat | undefined, value: string): string | null {
  const v = value.trim();
  if (!format || v === '') return null;
  switch (format) {
    case 'ip':
      return isIPv4(v) || isIPv6(v) ? null : 'Not an IP address';
    case 'ipv4':
      return isIPv4(v) ? null : 'Not an IPv4 address';
    case 'ipv6':
      return isIPv6(v) ? null : 'Not an IPv6 address';
    case 'cidr':
      return cidrError(v);
    case 'hostport':
      // Listener addresses may leave the host out (":80" = every address).
      return v.startsWith(':') ? hostPortError(`0.0.0.0${v}`) : hostPortError(v);
    case 'url':
      try {
        const u = new URL(v);
        return u.protocol === 'http:' || u.protocol === 'https:' ? null : 'An http:// or https:// URL';
      } catch {
        return 'Not a URL (https://…)';
      }
    case 'domain':
      return hostError(v);
    case 'upstream':
      return serverError(v);
    case 'email':
      return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v) ? null : 'Not an e-mail address';
    case 'date-time':
      return /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(v) && Number.isFinite(Date.parse(v)) ? null : 'An absolute date and time with a time zone is required';
  }
}
