// Public resolver presets for the new-upstream-group wizard: each provider
// with the variants (filtering levels) and transports it documents, turned
// into server entries in the syntax internal/upstream accepts. Addresses
// and host names were checked on 2026-10-11 against the providers' pages:
//
//   Cloudflare   https://developers.cloudflare.com/1.1.1.1/ip-addresses/
//                https://developers.cloudflare.com/1.1.1.1/setup/ (Families DoT/DoH)
//                https://developers.cloudflare.com/1.1.1.1/encryption/dns-over-tls/ (one.one.one.one)
//                https://developers.cloudflare.com/1.1.1.1/encryption/dns-over-https/make-api-requests/
//   Quad9        https://quad9.net/service/service-addresses-and-features/
//   Google       https://developers.google.com/speed/public-dns/docs/secure-transports
//   AdGuard DNS  https://adguard-dns.io/kb/general/dns-providers/ (also the source used
//                for OpenDNS / Cisco Umbrella, whose own support pages now redirect)
//   Mullvad      https://mullvad.net/en/help/dns-over-https-and-dns-over-tls
//                (encrypted only: "These IPs can only be used with DNS resolvers that
//                support DoH or DoT, not with DNS over UDP/53 or TCP/53")
//   NextDNS      https://help.nextdns.io (the profile ID is part of the DoT/DoQ host
//                name, the DoH path); plain DNS needs a linked IP and is not offered
//   Control D    https://docs.controld.com/docs/free-dns
//
// No provider above documents DNS over QUIC except AdGuard, NextDNS and
// Control D, so only they get the DoQ transport.

export type Transport = 'plain' | 'dot' | 'doh' | 'doq';

export const TRANSPORTS: { value: Transport; label: string; help: string }[] = [
  { value: 'doh', label: 'DNS over HTTPS (DoH)', help: 'Encrypted on port 443; looks like web traffic, rarely blocked.' },
  { value: 'dot', label: 'DNS over TLS (DoT)', help: 'Encrypted on port 853.' },
  { value: 'doq', label: 'DNS over QUIC (DoQ)', help: 'Encrypted over QUIC (UDP 853); low latency.' },
  { value: 'plain', label: 'Plain DNS', help: 'Unencrypted UDP/TCP 53; anyone on the path can read and change it.' },
];

export interface ProviderVariant {
  id: string;
  label: string;
  help: string;
  ipv4?: string[];
  ipv6?: string[];
  /** DoT and DoQ host name. */
  host?: string;
  /** DoQ host name when it differs from the DoT one. */
  quicHost?: string;
  doh?: string;
  /** Transports this variant documents (default: those its fields allow). */
  transports?: Transport[];
}

export interface Provider {
  id: string;
  name: string;
  description: string;
  homepage: string;
  /** Icon ids to look for in GET /catalogue/icons (dashboard-icons slugs). */
  icons: string[];
  variants: ProviderVariant[];
  /** NextDNS: the profile (configuration) ID goes into every address. */
  needsID?: { label: string; placeholder: string; pattern: RegExp; help: string };
}

export const PROVIDERS: Provider[] = [
  {
    id: 'cloudflare',
    name: 'Cloudflare',
    description: '1.1.1.1: fast, no filtering, logs purged within 24 hours.',
    homepage: 'https://one.one.one.one/',
    icons: ['cloudflare'],
    variants: [
      {
        id: 'standard',
        label: '1.1.1.1 (no filtering)',
        help: 'Answers everything.',
        ipv4: ['1.1.1.1', '1.0.0.1'],
        ipv6: ['2606:4700:4700::1111', '2606:4700:4700::1001'],
        host: 'one.one.one.one',
        doh: 'https://cloudflare-dns.com/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
      {
        id: 'security',
        label: 'Families: malware',
        help: 'Blocks malware and phishing.',
        ipv4: ['1.1.1.2', '1.0.0.2'],
        ipv6: ['2606:4700:4700::1112', '2606:4700:4700::1002'],
        host: 'security.cloudflare-dns.com',
        doh: 'https://security.cloudflare-dns.com/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
    ],
  },
  {
    id: 'cloudflare-family',
    name: 'Cloudflare Family',
    description: '1.1.1.1 for Families: blocks malware and adult content.',
    homepage: 'https://one.one.one.one/family/',
    icons: ['cloudflare'],
    variants: [
      {
        id: 'family',
        label: 'Malware and adult content',
        help: 'Blocks malware, phishing and adult content.',
        ipv4: ['1.1.1.3', '1.0.0.3'],
        ipv6: ['2606:4700:4700::1113', '2606:4700:4700::1003'],
        host: 'family.cloudflare-dns.com',
        doh: 'https://family.cloudflare-dns.com/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
    ],
  },
  {
    id: 'quad9',
    name: 'Quad9',
    description: 'Non-profit; blocks malicious domains and validates DNSSEC.',
    homepage: 'https://quad9.net/',
    icons: ['quad9'],
    variants: [
      {
        id: 'recommended',
        label: 'Malware blocking, DNSSEC',
        help: 'The recommended service.',
        ipv4: ['9.9.9.9', '149.112.112.112'],
        ipv6: ['2620:fe::fe', '2620:fe::9'],
        host: 'dns.quad9.net',
        doh: 'https://dns.quad9.net/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
    ],
  },
  {
    id: 'google',
    name: 'Google Public DNS',
    description: 'No filtering; validates DNSSEC.',
    homepage: 'https://developers.google.com/speed/public-dns',
    icons: ['google'],
    variants: [
      {
        id: 'standard',
        label: 'Google Public DNS',
        help: 'Answers everything.',
        ipv4: ['8.8.8.8', '8.8.4.4'],
        ipv6: ['2001:4860:4860::8888', '2001:4860:4860::8844'],
        host: 'dns.google',
        doh: 'https://dns.google/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
    ],
  },
  {
    id: 'adguard',
    name: 'AdGuard DNS',
    description: 'Blocks ads, trackers and phishing.',
    homepage: 'https://adguard-dns.io/',
    icons: ['adguard-home', 'adguard'],
    variants: [
      {
        id: 'default',
        label: 'Default (ads and trackers)',
        help: 'Blocks ads, trackers and phishing.',
        ipv4: ['94.140.14.14', '94.140.15.15'],
        ipv6: ['2a10:50c0::ad1:ff', '2a10:50c0::ad2:ff'],
        host: 'dns.adguard-dns.com',
        doh: 'https://dns.adguard-dns.com/dns-query',
      },
    ],
  },
  {
    id: 'adguard-family',
    name: 'AdGuard DNS Family',
    description: 'AdGuard DNS plus adult content blocking and safe search.',
    homepage: 'https://adguard-dns.io/',
    icons: ['adguard-home', 'adguard'],
    variants: [
      {
        id: 'family',
        label: 'Family protection',
        help: 'Ads, trackers, phishing and adult content; enforces safe search.',
        ipv4: ['94.140.14.15', '94.140.15.16'],
        ipv6: ['2a10:50c0::bad1:ff', '2a10:50c0::bad2:ff'],
        host: 'family.adguard-dns.com',
        doh: 'https://family.adguard-dns.com/dns-query',
      },
    ],
  },
  {
    id: 'mullvad',
    name: 'Mullvad DNS',
    description: 'Privacy-focused, encrypted only (no plain DNS).',
    homepage: 'https://mullvad.net/en/help/dns-over-https-and-dns-over-tls',
    icons: ['mullvad'],
    variants: [
      { id: 'dns', label: 'No filtering', help: 'dns.mullvad.net', host: 'dns.mullvad.net', doh: 'https://dns.mullvad.net/dns-query', transports: ['dot', 'doh'] },
      { id: 'adblock', label: 'Ads and trackers', help: 'adblock.dns.mullvad.net', host: 'adblock.dns.mullvad.net', doh: 'https://adblock.dns.mullvad.net/dns-query', transports: ['dot', 'doh'] },
      { id: 'base', label: 'Ads, trackers and malware', help: 'base.dns.mullvad.net', host: 'base.dns.mullvad.net', doh: 'https://base.dns.mullvad.net/dns-query', transports: ['dot', 'doh'] },
      {
        id: 'extended',
        label: 'Base and social media',
        help: 'extended.dns.mullvad.net',
        host: 'extended.dns.mullvad.net',
        doh: 'https://extended.dns.mullvad.net/dns-query',
        transports: ['dot', 'doh'],
      },
      { id: 'family', label: 'Base, adult content and gambling', help: 'family.dns.mullvad.net', host: 'family.dns.mullvad.net', doh: 'https://family.dns.mullvad.net/dns-query', transports: ['dot', 'doh'] },
      { id: 'all', label: 'Everything above', help: 'all.dns.mullvad.net', host: 'all.dns.mullvad.net', doh: 'https://all.dns.mullvad.net/dns-query', transports: ['dot', 'doh'] },
    ],
  },
  {
    id: 'nextdns',
    name: 'NextDNS',
    description: 'Your own filtering profile, configured at my.nextdns.io.',
    homepage: 'https://nextdns.io/',
    icons: ['nextdns'],
    needsID: {
      label: 'Profile ID',
      placeholder: 'abc123',
      pattern: /^[a-z0-9]{6}$/,
      help: 'The six-character ID on the Setup tab of my.nextdns.io.',
    },
    variants: [{ id: 'profile', label: 'Your profile', help: 'Filtering as configured in the NextDNS profile.', host: '{id}.dns.nextdns.io', doh: 'https://dns.nextdns.io/{id}', transports: ['dot', 'doh', 'doq'] }],
  },
  {
    id: 'controld',
    name: 'Control D',
    description: 'Free resolvers with fixed filter levels.',
    homepage: 'https://controld.com/free-dns',
    icons: ['control-d', 'controld'],
    variants: [
      {
        id: 'p0',
        label: 'Unfiltered',
        help: 'Answers everything.',
        ipv4: ['76.76.2.0', '76.76.10.0'],
        ipv6: ['2606:1a40::', '2606:1a40:1::'],
        host: 'p0.freedns.controld.com',
        doh: 'https://freedns.controld.com/p0',
      },
      {
        id: 'p1',
        label: 'Malware',
        help: 'Blocks malware.',
        ipv4: ['76.76.2.1', '76.76.10.1'],
        ipv6: ['2606:1a40::1', '2606:1a40:1::1'],
        host: 'p1.freedns.controld.com',
        doh: 'https://freedns.controld.com/p1',
      },
      {
        id: 'p2',
        label: 'Ads and tracking',
        help: 'Malware, ads and trackers.',
        ipv4: ['76.76.2.2', '76.76.10.2'],
        ipv6: ['2606:1a40::2', '2606:1a40:1::2'],
        host: 'p2.freedns.controld.com',
        doh: 'https://freedns.controld.com/p2',
      },
      {
        id: 'p3',
        label: 'Social',
        help: 'Malware, ads, trackers and social networks.',
        ipv4: ['76.76.2.3', '76.76.10.3'],
        ipv6: ['2606:1a40::3', '2606:1a40:1::3'],
        host: 'p3.freedns.controld.com',
        doh: 'https://freedns.controld.com/p3',
      },
      {
        id: 'family',
        label: 'Family friendly',
        help: 'Also adult content and drugs.',
        ipv4: ['76.76.2.4', '76.76.10.4'],
        ipv6: ['2606:1a40::4', '2606:1a40:1::4'],
        host: 'family.freedns.controld.com',
        doh: 'https://freedns.controld.com/family',
      },
    ],
  },
  {
    id: 'opendns',
    name: 'OpenDNS',
    description: 'Cisco Umbrella home resolvers.',
    homepage: 'https://www.opendns.com/',
    icons: ['opendns', 'cisco'],
    variants: [
      {
        id: 'standard',
        label: 'Standard',
        help: 'Blocks phishing.',
        ipv4: ['208.67.222.222', '208.67.220.220'],
        ipv6: ['2620:119:35::35', '2620:119:53::53'],
        host: 'dns.opendns.com',
        doh: 'https://doh.opendns.com/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
      {
        id: 'familyshield',
        label: 'FamilyShield',
        help: 'Also blocks adult content.',
        ipv4: ['208.67.222.123', '208.67.220.123'],
        host: 'familyshield.opendns.com',
        doh: 'https://doh.familyshield.opendns.com/dns-query',
        transports: ['plain', 'dot', 'doh'],
      },
    ],
  },
];

/** The transports a variant offers, in TRANSPORTS order. */
export function transportsOf(v: ProviderVariant): Transport[] {
  const has: Record<Transport, boolean> = { plain: !!v.ipv4?.length, dot: !!v.host, doh: !!v.doh, doq: !!(v.quicHost ?? v.host) };
  const allowed = v.transports ?? (['plain', 'dot', 'doh', 'doq'] as Transport[]);
  return TRANSPORTS.map((t) => t.value).filter((t) => has[t] && allowed.includes(t));
}

/**
 * The server entries of a variant over a transport, each with a display
 * name so the query log and metrics read "quad9-dot" rather than a URL.
 * Plain DNS lists the IPv4 addresses, and the IPv6 ones when asked for.
 */
export function serversFor(p: Provider, v: ProviderVariant, t: Transport, opts: { ipv6?: boolean; id?: string } = {}): string[] {
  const fill = (s: string) => s.replace('{id}', opts.id ?? '');
  const base = p.variants.length > 1 ? `${p.id}-${v.id}` : p.id;
  switch (t) {
    case 'plain': {
      const addrs = [...(v.ipv4 ?? []), ...(opts.ipv6 ? (v.ipv6 ?? []) : [])];
      return addrs.map((a, i) => `${base}-${i + 1}=${a.includes(':') ? `[${a}]:53` : `${a}:53`}`);
    }
    case 'dot':
      return v.host ? [`${base}-dot=tls://${fill(v.host)}`] : [];
    case 'doq':
      return (v.quicHost ?? v.host) ? [`${base}-doq=quic://${fill(v.quicHost ?? v.host ?? '')}`] : [];
    case 'doh':
      return v.doh ? [`${base}-doh=${fill(v.doh)}`] : [];
  }
}

/** A group name proposal for a preset ("quad9", "mullvad-adblock"). */
export function groupNameFor(p: Provider, v: ProviderVariant): string {
  return p.variants.length > 1 ? `${p.id}-${v.id}` : p.id;
}
