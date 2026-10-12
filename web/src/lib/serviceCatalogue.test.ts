import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import type { CatalogueService } from '../api/types';
import { contentList, contentListName, ensureContentLists, missingContentLists } from './contentCategories';
import { CURATED_LISTS } from './lists';
import {
  categoryCounts,
  effectiveServices,
  groupForGrid,
  matchesSearch,
  missingRequirements,
  serviceState,
  setCategory,
  tabLabel,
  toggleService,
  type ServiceSelection,
} from './serviceCatalogue';

const s = (id: string, category: string, domains: string[] = [`${id}.example`], requires?: string[]): CatalogueService => ({
  id,
  name: id.toUpperCase(),
  category,
  list: `service:${id}`,
  rules: domains.length,
  domains: domains.map((d) => `||${d}^`),
  source: 'test',
  ...(requires ? { requires } : {}),
});
const services = [s('chat', 'ai', ['chat.example', 'api.chat.example']), s('paint', 'ai'), s('tube', 'video'), s('game', 'gaming', undefined, ['store']), s('store', 'gaming')];
const cats = [
  { id: 'ai', name: 'AI', order: 1, services: 2 },
  { id: 'video', name: 'Video', order: 2, services: 1 },
  { id: 'gaming', name: 'Gaming', order: 3, services: 2 },
];
const sel = (blocked: string[] = [], categories: string[] = [], allowed: string[] = []): ServiceSelection => ({ blocked, categories, allowed });

describe('service selection', () => {
  it('derives each service state and the effective set like the server', () => {
    const v = sel(['tube', 'paint'], ['ai'], ['paint', 'chat']);
    expect(serviceState(services[0]!, v)).toBe('exception');
    expect(serviceState(services[1]!, v)).toBe('blocked'); // blockedServices wins over an exception
    expect(serviceState(services[2]!, v)).toBe('blocked');
    expect(serviceState(services[3]!, v)).toBe('off');
    expect(effectiveServices(sel(['tube'], ['ai'], ['paint']), services)).toEqual(['tube', 'chat']);
  });

  it('toggles a service in and out of blocked, category and exception', () => {
    const chat = services[0]!;
    expect(toggleService(chat, sel())).toEqual(sel(['chat']));
    expect(toggleService(chat, sel(['chat']))).toEqual(sel());
    expect(toggleService(chat, sel([], ['ai']))).toEqual(sel([], ['ai'], ['chat']));
    expect(toggleService(chat, sel([], ['ai'], ['chat']))).toEqual(sel([], ['ai']));
    // Listed and in a blocked category: unblocking takes both away.
    expect(toggleService(chat, sel(['chat'], ['ai']))).toEqual(sel([], ['ai'], ['chat']));
  });

  it('switches a category and drops its exceptions', () => {
    expect(setCategory('ai', true, services, sel(['tube'], [], ['chat']))).toEqual(sel(['tube'], ['ai']));
    expect(setCategory('ai', false, services, sel(['chat'], ['ai', 'video'], ['paint']))).toEqual(sel(['chat'], ['video']));
  });

  it('groups, counts and filters for the grid', () => {
    const v = sel([], ['ai'], ['paint']);
    expect(groupForGrid(services, cats, v, { tab: 'all', search: '', blockedOnly: false }).map((g) => [g.category.id, g.total, g.blocked])).toEqual([
      ['ai', 2, 1],
      ['video', 1, 0],
      ['gaming', 2, 0],
    ]);
    expect(groupForGrid(services, cats, v, { tab: 'all', search: '', blockedOnly: true }).map((g) => g.services.map((x) => x.id))).toEqual([['chat']]);
    expect(groupForGrid(services, cats, v, { tab: 'gaming', search: '', blockedOnly: false }).map((g) => g.category.id)).toEqual(['gaming']);
    expect(groupForGrid(services, cats, v, { tab: 'all', search: 'api.chat', blockedOnly: false }).map((g) => g.services.map((x) => x.id))).toEqual([['chat']]);
    expect(matchesSearch(services[2]!, 'TUBE')).toBe(true);
    expect(categoryCounts(services, v).get('ai')).toEqual({ total: 2, blocked: 1 });
    expect(tabLabel('AI', 18, 3)).toBe('AI · 18 · 3 blocked');
    expect(tabLabel('AI', 18, 0)).toBe('AI · 18');
    // A category the category list lacks is appended with a made-up name.
    expect(groupForGrid([...services, s('x', 'retro-games')], cats, v, { tab: 'all', search: '', blockedOnly: false }).at(-1)?.category.name).toBe('Retro games');
  });

  it('finds blocked services whose requirements are not blocked', () => {
    expect(missingRequirements(services, sel(['game'])).map((h) => [h.service.id, h.missing.map((m) => m.id)])).toEqual([['game', ['store']]]);
    expect(missingRequirements(services, sel(['game'], ['gaming']))).toEqual([]);
  });
});

describe('content categories', () => {
  it('are offered as well-known lists with their licence', () => {
    const gambling = CURATED_LISTS.find((c) => c.id === 'blp-gambling');
    expect(gambling).toMatchObject({ publisher: 'Block List Project', format: 'domains', url: 'https://blocklistproject.github.io/Lists/alt-version/gambling-nl.txt', group: 'blp' });
    expect(gambling?.licence).toContain('GPL-3.0');
    expect(CURATED_LISTS.find((c) => c.id === 'blp-phishing')?.licence).toBe('Unlicense');
    // OISD is GPL-3.0 (oisd.nl FAQ, the repository's LICENSE).
    expect(CURATED_LISTS.find((c) => c.id === 'oisd-big')?.licence).toBe('GPL-3.0');
  });

  it('creates the missing BlockLists of a policy, tolerating a concurrent create', async () => {
    const existing = [contentList({ id: 'gambling', name: 'Gambling', group: 'content', description: '', file: 'gambling', size: '' }, 'home')];
    const refs = ['ads-list', contentListName('gambling'), contentListName('porn'), contentListName('malware')];
    expect(missingContentLists(refs, existing, 'home').map((c) => c.id)).toEqual(['porn', 'malware']);
    const create = vi.fn((_k: 'blocklists', obj: { metadata: { name: string } }) =>
      obj.metadata.name === 'blp-malware' ? Promise.reject(new ApiError(409, 'conflict', 'exists')) : Promise.resolve(obj),
    );
    const api = { objects: { list: vi.fn(() => Promise.resolve({ items: existing })), create } };
    expect(await ensureContentLists(api, 'home', refs)).toEqual(['blp-porn']);
    const porn = create.mock.calls[0]![1] as ReturnType<typeof contentList>;
    expect(porn.metadata).toMatchObject({ name: 'blp-porn', namespace: 'home' });
    expect(porn.spec.sources).toEqual([{ name: 'blocklistproject', url: 'https://blocklistproject.github.io/Lists/alt-version/porn-nl.txt', format: 'domains' }]);
    // Policies without content categories cost no request.
    const quiet = { objects: { list: vi.fn(), create: vi.fn() } };
    expect(await ensureContentLists(quiet, 'home', ['ads-list'])).toEqual([]);
    expect(quiet.objects.list).not.toHaveBeenCalled();
  });
});
