// Content categories: the Block List Project's topic lists
// (https://github.com/blocklistproject/Lists), offered as well-known lists
// in the list wizard and as categories a policy turns on. Nothing is
// embedded: a category is a BlockList "blp-<id>" with the upstream URL as
// its one source, created on demand in the policy's namespace and
// refreshed on the normal list schedule (blocking.refreshInterval).
//
// Checked at commit ee3bdbaddf49c756cf3340bb2e6a9dee5282d1c6 (main,
// 2026-07-20) of blocklistproject/Lists: the repository's LICENSE is the
// Unlicense (public domain); the list files' headers say "License: MIT";
// several lists merge third-party upstream lists (config/lists.yml), the
// gambling and porn lists among them HaGeZi's (GPL-3.0). kudurru only
// names the URLs; each instance downloads the lists itself. The files are
// the "No IP" (one domain per line) variants under alt-version/.
import { ApiError } from '../api/client';
import type { AnyObject, BlockList } from '../api/types';

export const BLP_COMMIT = 'ee3bdbaddf49c756cf3340bb2e6a9dee5282d1c6';
export const BLP_REPO = 'https://github.com/blocklistproject/Lists';

export const BLP_LICENCE_NOTE =
  'The Block List Project publishes its repository under the Unlicense (public domain); some lists merge third-party lists, the gambling and adult ones among them HaGeZi’s (GPL-3.0). kudurru does not ship them: this server downloads them.';

export type ContentGroup = 'content' | 'security' | 'privacy';

export const CONTENT_GROUPS: { id: ContentGroup; name: string }[] = [
  { id: 'content', name: 'Content' },
  { id: 'security', name: 'Security' },
  { id: 'privacy', name: 'Ads and tracking' },
];

export interface ContentCategory {
  /** Stable ID; the BlockList is "blp-<id>". */
  id: string;
  name: string;
  group: ContentGroup;
  description: string;
  /** The upstream file name without "-nl.txt". */
  file: string;
  /** Rough size at the checked commit, for the choice. */
  size: string;
  /** Upstream lists it merges, when the project's config names any. */
  merges?: string;
}

export const CONTENT_CATEGORIES: ContentCategory[] = [
  { id: 'gambling', name: 'Gambling', group: 'content', description: 'Betting, casino and lottery sites.', file: 'gambling', size: '~280k names', merges: 'HaGeZi gambling (GPL-3.0), ShadowWhisperer' },
  { id: 'porn', name: 'Adult content', group: 'content', description: 'Pornography and other adult sites.', file: 'porn', size: '~950k names', merges: 'HaGeZi NSFW (GPL-3.0), ShadowWhisperer, zachlagden' },
  { id: 'drugs', name: 'Drugs', group: 'content', description: 'Sites selling or promoting illegal drugs.', file: 'drugs', size: '~26k names' },
  { id: 'vaping', name: 'Vaping', group: 'content', description: 'Vape and e-cigarette shops.', file: 'vaping', size: '~120 names' },
  { id: 'piracy', name: 'Piracy', group: 'content', description: 'Illegal streaming and download sites.', file: 'piracy', size: '~2k names' },
  { id: 'torrent', name: 'Torrent sites', group: 'content', description: 'Torrent indexes and trackers.', file: 'torrent', size: '~2.6k names' },
  { id: 'malware', name: 'Malware', group: 'security', description: 'Hosts that distribute malware. Very large: about 2.6 million names, so it needs several hundred MB of memory.', file: 'malware', size: '~2.6M names', merges: 'ShadowWhisperer, zachlagden' },
  { id: 'phishing', name: 'Phishing', group: 'security', description: 'Sites that imitate others to steal credentials.', file: 'phishing', size: '~190k names' },
  { id: 'scam', name: 'Scams', group: 'security', description: 'Scam shops and fake services.', file: 'scam', size: '~8.5k names', merges: 'ShadowWhisperer' },
  { id: 'fraud', name: 'Fraud', group: 'security', description: 'Fraudulent sites.', file: 'fraud', size: '~285k names', merges: 'zachlagden' },
  { id: 'ransomware', name: 'Ransomware', group: 'security', description: 'Ransomware command-and-control and distribution hosts.', file: 'ransomware', size: '~1.9k names' },
  { id: 'crypto', name: 'Cryptojacking', group: 'security', description: 'In-browser crypto miners and crypto scams.', file: 'crypto', size: '~1.3k names', merges: 'ShadowWhisperer' },
  { id: 'redirect', name: 'Malicious redirects', group: 'security', description: 'URL shorteners and redirectors used to hide destinations.', file: 'redirect', size: '~110k names' },
  { id: 'ads', name: 'Ads', group: 'privacy', description: 'Ad servers.', file: 'ads', size: '~235k names', merges: 'ShadowWhisperer, zachlagden' },
  { id: 'tracking', name: 'Tracking', group: 'privacy', description: 'Tracking and analytics hosts.', file: 'tracking', size: '~145k names', merges: 'ShadowWhisperer, zachlagden' },
  { id: 'smart-tv', name: 'Smart TV telemetry', group: 'privacy', description: 'Telemetry of smart TVs (a beta list).', file: 'smart-tv', size: '~90 names' },
];

/** The list URL of a category (the project's GitHub Pages, one domain per line). */
export function contentURL(c: ContentCategory): string {
  return `https://blocklistproject.github.io/Lists/alt-version/${c.file}-nl.txt`;
}

export const CONTENT_PREFIX = 'blp-';

export function contentListName(id: string): string {
  return CONTENT_PREFIX + id;
}

export function contentCategoryOfList(name: string): ContentCategory | undefined {
  return name.startsWith(CONTENT_PREFIX) ? CONTENT_CATEGORIES.find((c) => contentListName(c.id) === name) : undefined;
}

/** The BlockList a content category is (what ensureContentLists creates). */
export function contentList(c: ContentCategory, namespace: string): BlockList {
  return {
    apiVersion: 'kudurru.io/v1alpha1',
    kind: 'BlockList',
    metadata: {
      name: contentListName(c.id),
      namespace,
      annotations: { 'kudurru.io/comment': `Content category ${c.name}: Block List Project (${BLP_REPO}), downloaded from its URL` },
    },
    spec: { mode: 'block', sources: [{ name: 'blocklistproject', url: contentURL(c), format: 'domains' }] },
  };
}

/** The content categories refs names whose BlockList does not exist yet in namespace. */
export function missingContentLists(refs: string[], existing: AnyObject[], namespace: string): ContentCategory[] {
  const have = new Set(existing.filter((o) => (o.metadata.namespace ?? 'default') === namespace).map((o) => o.metadata.name));
  return refs.map(contentCategoryOfList).filter((c): c is ContentCategory => !!c && !have.has(contentListName(c.id)));
}

interface ListApi {
  objects: {
    list: (kind: 'blocklists') => Promise<{ items: AnyObject[] }>;
    create: (kind: 'blocklists', obj: BlockList) => Promise<unknown>;
  };
}

/**
 * Creates the BlockLists of the content categories refs names that do not
 * exist yet in namespace; returns the names it created. Call it before
 * writing a policy whose blockListRefs may name new ones.
 */
export async function ensureContentLists(api: ListApi, namespace: string, refs: string[] | undefined): Promise<string[]> {
  if (!refs?.some((r) => contentCategoryOfList(r))) return [];
  const existing = (await api.objects.list('blocklists')).items;
  const created: string[] = [];
  for (const c of missingContentLists(refs, existing, namespace)) {
    try {
      await api.objects.create('blocklists', contentList(c, namespace));
      created.push(contentListName(c.id));
    } catch (err) {
      // Another tab or policy created it meanwhile: use that one.
      if (!(err instanceof ApiError && err.status === 409)) throw err;
    }
  }
  return created;
}
