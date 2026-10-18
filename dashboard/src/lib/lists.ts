// BlockList helpers shared by the Lists table, the list page, the new-list
// wizard and the Domains page: source names and kinds as the server derives
// them (internal/blocking SourceName), per-source enable (ux-plan C2), the
// typed choices of a list's fields and the curated set of public lists the
// wizard offers.
import type { BlockList, ListFormat, ListSource, SourceStatus } from '../api/types';
import type { EnumOption } from '../components/fields/EnumSelect';
import { CONTENT_CATEGORIES, contentListName, contentURL } from './contentCategories';
import { formatExact } from './format';
import { listEntryError } from './objectSchemas';

/** entries: the structured entries of the dashboard's allow and deny lists (ui-*). */
export type SourceKind = 'url' | 'inline' | 'configMap' | 'entries';

export function sourceKind(src: ListSource): SourceKind {
  if (src.url) return 'url';
  if (src.configMapRef) return 'configMap';
  if (src.entries) return 'entries';
  return 'inline';
}

/** "N of M entries enabled" when some are switched off, else "N" (status.sources[].entries of entries + excluded). */
export function enabledEntriesText(st: Pick<SourceStatus, 'entries' | 'excluded'> | undefined): string {
  const on = st?.entries ?? 0;
  const off = st?.excluded ?? 0;
  if (!off) return formatExact(on);
  return `${formatExact(on)} of ${formatExact(on + off)} enabled`;
}

/**
 * The name status.sources and the entries API use for source i: its name,
 * else its URL, else configmap/<name>, else inline-<i> (blocking.SourceName).
 */
export function sourceName(i: number, src: ListSource): string {
  if (src.name) return src.name;
  if (src.url) return src.url;
  if (src.configMapRef) return `configmap/${src.configMapRef.name}`;
  return `inline-${i}`;
}

/** spec.sources[].enabled: omitted means enabled. */
export function sourceEnabled(src: ListSource): boolean {
  return src.enabled !== false;
}

export function sourceStatus(list: BlockList, i: number): SourceStatus | undefined {
  const src = list.spec.sources[i];
  if (!src) return undefined;
  const name = sourceName(i, src);
  return list.status?.sources?.find((s) => s.name === name);
}

/** "3 of 4 enabled", "1 source" or "2 sources". */
export function sourcesSummary(sources: ListSource[]): string {
  const on = sources.filter(sourceEnabled).length;
  if (on < sources.length) return `${on} of ${sources.length} enabled`;
  return sources.length === 1 ? '1 source' : `${sources.length} sources`;
}

/** A copy of the list with source i replaced by patch (undefined keys are removed). */
export function withSource(list: BlockList, i: number, patch: Partial<ListSource>): BlockList {
  const sources = list.spec.sources.map((s, j) => {
    if (j !== i) return s;
    const next: ListSource = { ...s, ...patch };
    for (const k of Object.keys(next) as (keyof ListSource)[]) if (next[k] === undefined) delete next[k];
    return next;
  });
  return { ...list, spec: { ...list.spec, sources } };
}

/** Sets spec.sources[i].enabled, omitting the field when enabled (the default). */
export function withSourceEnabled(list: BlockList, i: number, enabled: boolean): BlockList {
  return withSource(list, i, { enabled: enabled ? undefined : false });
}

export function listNamespace(l: Pick<BlockList, 'metadata'>): string {
  return l.metadata.namespace ?? 'default';
}

/** The list page of a list. */
export function listPath(l: Pick<BlockList, 'metadata'>): string {
  return `/lists/${encodeURIComponent(listNamespace(l))}/${encodeURIComponent(l.metadata.name)}`;
}

/** The react-query key of one list (under qk.objects('blocklists') so list invalidations reach it). */
export function listKey(namespace: string, name: string) {
  return ['objects', 'blocklists', namespace, name] as const;
}

export const MODE_OPTIONS: EnumOption[] = [
  { value: 'block', label: 'Block list', help: 'Matching names are blocked for the client groups of the policies that use the list.' },
  { value: 'allow', label: 'Allow list', help: 'Matching names are never blocked; allow lists win over block lists in the same policy.' },
];

export const FORMAT_OPTIONS: EnumOption[] = [
  { value: 'auto', label: 'Detect automatically', help: 'kudurru reads the first lines and picks hosts, domains, adblock or regex.' },
  { value: 'hosts', label: 'hosts file', help: '"0.0.0.0 ads.example.com" lines; other addresses are rewrites and count as unsupported.' },
  { value: 'domains', label: 'One domain per line', help: 'Plain names; "*.example.com" also matches example.com.' },
  { value: 'adblock', label: 'Adblock syntax', help: '"||ads.example.com^" rules and "@@" exceptions (AdGuard, uBlock, OISD, HaGeZi).' },
  { value: 'regex', label: 'Regular expressions', help: 'One RE2 expression per line, matched against the whole name.' },
];

export const RESPONSE_OPTIONS: EnumOption[] = [
  { value: 'nxdomain', label: 'NXDOMAIN', help: 'The name does not exist.' },
  { value: 'zeroIP', label: '0.0.0.0 / ::', help: 'An unroutable address; apps fail fast.' },
  { value: 'refused', label: 'REFUSED', help: 'The server refuses to answer.' },
  { value: 'customIP', label: 'Custom IP', help: 'The addresses below, e.g. a local web server.' },
  { value: 'blockPage', label: 'Block page', help: 'Browsers land on the kudurru block page explaining the block (Settings → Block page).' },
];

/** Labels of a format value for tables. */
export function formatLabel(f: ListFormat | undefined): string {
  return FORMAT_OPTIONS.find((o) => o.value === (f ?? 'auto'))?.label ?? f ?? 'auto';
}

/**
 * A format guess from a list URL, for preselecting the Format field; the
 * server still detects "auto" itself. Known publishers first, then the file
 * name.
 */
export function guessFormat(url: string): ListFormat {
  const u = url.trim().toLowerCase();
  if (!u) return 'auto';
  if (/oisd\.nl\/?$|oisd\.nl\/(abp|adblock)/.test(u) || /\/adblock\//.test(u) || /adguard|easylist|\.abp|abp\.txt/.test(u)) return 'adblock';
  if (/oisd\.nl\/domainswild|\/wildcard\/|\/domains\//.test(u) || /domains?\.txt$/.test(u)) return 'domains';
  if (/(^|[/._-])hosts([/._-]|$)/.test(u)) return 'hosts';
  if (/regex/.test(u)) return 'regex';
  return 'auto';
}

/** The entries of pasted or uploaded text: trimmed lines without blanks and comments. */
export function parseEntries(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('#') && !l.startsWith('!'));
}

/** Why one entry of a source with this format would not parse (null when it does). */
export function entryError(format: ListFormat | undefined, e: string): string | null {
  if (format !== 'regex') return listEntryError(e);
  try {
    // nosemgrep: detect-non-literal-regexp -- compiled to validate the input, never run
    new RegExp(e.replace(/^\/(.*)\/$/, '$1'));
    return null;
  } catch (err) {
    return `Invalid regular expression: ${(err as Error).message}`;
  }
}

/** The first invalid entry and the number of invalid ones. */
export function entriesProblem(entries: string[], format?: ListFormat): { first: string; error: string; count: number } | null {
  let first: { first: string; error: string } | null = null;
  let count = 0;
  for (const e of entries) {
    const err = entryError(format, e);
    if (!err) continue;
    count++;
    first ??= { first: e, error: err };
  }
  return first ? { ...first, count } : null;
}

export function isHTTPURL(v: string): boolean {
  return /^https?:\/\/[^\s/]+\S*$/.test(v.trim());
}

/** A well-known public list the new-list wizard offers (names and URLs only; nothing is embedded). */
export interface CuratedList {
  id: string;
  name: string;
  publisher: string;
  url: string;
  format: ListFormat;
  licence: string;
  description: string;
  /** Rough size, for the choice. */
  size: string;
  /** Suggested source name. */
  source: string;
  /** The wizard's group: general lists, or the Block List Project's topic lists (content categories). */
  group?: 'blp';
}

export const CURATED_LISTS: CuratedList[] = [
  {
    id: 'stevenblack-unified',
    name: 'Unified hosts',
    publisher: 'Steven Black',
    url: 'https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts',
    format: 'hosts',
    licence: 'MIT',
    description: 'Adware and malware hosts merged from several curated sources. Conservative; rarely breaks sites.',
    size: '~80k names',
    source: 'stevenblack',
  },
  {
    id: 'stevenblack-family',
    name: 'Unified hosts + fake news, gambling, porn',
    publisher: 'Steven Black',
    url: 'https://raw.githubusercontent.com/StevenBlack/hosts/master/alternates/fakenews-gambling-porn/hosts',
    format: 'hosts',
    licence: 'MIT',
    description: 'The unified list plus the fake news, gambling and adult extensions; a family-network starting point.',
    size: '~200k names',
    source: 'stevenblack-family',
  },
  {
    id: 'oisd-small',
    name: 'OISD small',
    publisher: 'oisd.nl',
    url: 'https://small.oisd.nl/',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: 'Ads, tracking and malware with very few false positives. Good for shared networks.',
    size: '~50k rules',
    source: 'oisd-small',
  },
  {
    id: 'oisd-big',
    name: 'OISD big',
    publisher: 'oisd.nl',
    url: 'https://big.oisd.nl/',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: 'The full OISD list: broader coverage of ads, tracking, malware and phishing.',
    size: '~250k rules',
    source: 'oisd-big',
  },
  {
    id: 'hagezi-light',
    name: 'Multi LIGHT',
    publisher: 'HaGeZi',
    url: 'https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/light.txt',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: 'Basic protection against ads, tracking and malware; for networks where nothing may break.',
    size: '~100k rules',
    source: 'hagezi-light',
  },
  {
    id: 'hagezi-pro',
    name: 'Multi PRO',
    publisher: 'HaGeZi',
    url: 'https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/pro.txt',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: 'Extended protection; blocks more trackers and may need an allow entry now and then.',
    size: '~300k rules',
    source: 'hagezi-pro',
  },
  {
    id: 'hagezi-tif',
    name: 'Threat Intelligence Feeds',
    publisher: 'HaGeZi',
    url: 'https://raw.githubusercontent.com/hagezi/dns-blocklists/main/adblock/tif.txt',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: 'Malware, phishing, scam and command-and-control domains from public threat feeds.',
    size: '~600k rules',
    source: 'hagezi-tif',
  },
  {
    id: 'adguard-dns',
    name: 'AdGuard DNS filter',
    publisher: 'AdGuard',
    url: 'https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt',
    format: 'adblock',
    licence: 'GPL-3.0',
    description: "The filter of AdGuard's public DNS: EasyList, EasyPrivacy and AdGuard rules reduced to DNS-level blocking.",
    size: '~60k rules',
    source: 'adguard-dns',
  },
];

// The Block List Project's topic lists (the content categories policies
// can turn on, lib/contentCategories.ts), as well-known lists too.
CURATED_LISTS.push(
  ...CONTENT_CATEGORIES.map(
    (c): CuratedList => ({
      id: contentListName(c.id),
      name: c.name,
      publisher: 'Block List Project',
      url: contentURL(c),
      format: 'domains',
      licence: c.merges?.includes('GPL') ? 'Unlicense, merges GPL-3.0' : 'Unlicense',
      description: `${c.description}${c.merges ? ` Merges ${c.merges}.` : ''}`,
      size: c.size,
      source: contentListName(c.id),
      group: 'blp',
    }),
  ),
);
