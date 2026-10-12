// The blocked-services picker (D60): every service of the catalogue
// (GET /catalogue/services) as a logo tile, grouped by category
// (GET /catalogue/categories) under a tab strip with counts ("AI · 19 · 3
// blocked"), a search over names and domains and a "Blocked only" filter.
// A tile is a toggle button (aria-pressed); each category has a "Block the
// whole category" switch (spec.blockedCategories), under which a click on
// a tile makes an exception (spec.allowedServices). Requires hints
// ("Fortnite needs Epic Games") offer to block the other service, and
// services of the opt-in AdGuard import carry their licence. IDs the
// catalogue in service does not have stay listed, so saving never drops
// them silently. The safe-search engines get the same logos, read-only
// (EngineList).
import { Button } from '@astryxdesign/core/Button';
import { Grid } from '@astryxdesign/core/Grid';
import { Heading } from '@astryxdesign/core/Heading';
import { Icon } from '@astryxdesign/core/Icon';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Tab, TabList } from '@astryxdesign/core/TabList';
import { Text } from '@astryxdesign/core/Text';
import { ToggleButton } from '@astryxdesign/core/ToggleButton';
import { Token } from '@astryxdesign/core/Token';
import { useQuery } from '@tanstack/react-query';
import {
  Bitcoin,
  Cloud,
  Dices,
  Gamepad2,
  Heart,
  MessageCircle,
  MessagesSquare,
  Music,
  Newspaper,
  Shapes,
  Shield,
  ShoppingCart,
  Sparkles,
  Tag,
  Tv,
  Users,
} from 'lucide-react';
import { useId, useMemo, useState, type ComponentType, type SVGProps } from 'react';
import { useApi } from '../api/context';
import type { CatalogueCategory, CatalogueService, SafeSearchEngine } from '../api/types';
import {
  categoryCounts,
  groupForGrid,
  isBlocked,
  missingRequirements,
  orderedCategories,
  serviceState,
  setCategory,
  tabLabel,
  toggleService,
  type ServiceSelection,
  type ServiceState,
} from '../lib/serviceCatalogue';
import { errorText } from './Feedback';
import { SwitchField, TextField } from './fields/TextField';
import { ServiceIcon } from './ServiceIcon';

/** Query keys of the catalogue (shared with the policy pages). */
export const catalogueServicesKey = ['catalogue', 'services'] as const;
export const catalogueCategoriesKey = ['catalogue', 'categories'] as const;
export const catalogueSafeSearchKey = ['catalogue', 'safesearch'] as const;
export const catalogueInfoKey = ['catalogue', 'info'] as const;

export function useCatalogueServices() {
  const api = useApi();
  return useQuery({ queryKey: catalogueServicesKey, queryFn: () => api.catalogue.services(), staleTime: 5 * 60_000 });
}

export function useCatalogueCategories() {
  const api = useApi();
  return useQuery({ queryKey: catalogueCategoriesKey, queryFn: () => api.catalogue.categories(), staleTime: 5 * 60_000, retry: false });
}

export function useSafeSearchEngines() {
  const api = useApi();
  return useQuery({ queryKey: catalogueSafeSearchKey, queryFn: () => api.catalogue.safeSearch(), staleTime: 5 * 60_000 });
}

/** The Lucide icons the catalogue's categories name (catalogue.json `icon`); others draw a tag. */
const CATEGORY_ICONS: Record<string, ComponentType<SVGProps<SVGSVGElement>>> = {
  sparkles: Sparkles,
  users: Users,
  'message-circle': MessageCircle,
  tv: Tv,
  music: Music,
  'gamepad-2': Gamepad2,
  'shopping-cart': ShoppingCart,
  heart: Heart,
  dices: Dices,
  newspaper: Newspaper,
  'messages-square': MessagesSquare,
  cloud: Cloud,
  shield: Shield,
  bitcoin: Bitcoin,
  shapes: Shapes,
};

export function CategoryIcon({ icon }: { icon?: string | undefined }) {
  return <Icon icon={(icon && CATEGORY_ICONS[icon]) || Tag} size="sm" color="secondary" />;
}

const STATE_TEXT: Record<ServiceState, string> = {
  blocked: 'Blocked',
  category: 'Blocked by category',
  exception: 'Allowed (exception)',
  off: '',
};

export interface ServiceGridProps {
  label: string;
  value: ServiceSelection;
  onChange: (sel: ServiceSelection) => void;
  isDisabled?: boolean;
  description?: string;
  /** The surrounding section already names the field. */
  isLabelHidden?: boolean;
}

export function ServiceGrid({ label, value, onChange, isDisabled, description, isLabelHidden }: ServiceGridProps) {
  const q = useCatalogueServices();
  const cq = useCatalogueCategories();
  const [tab, setTab] = useState('all');
  const [search, setSearch] = useState('');
  const [blockedOnly, setBlockedOnly] = useState(false);
  const panelId = useId();
  const items = useMemo(() => q.data?.items ?? [], [q.data]);
  const categories = useMemo(() => orderedCategories(cq.data?.items ?? [], items), [cq.data, items]);
  const counts = useMemo(() => categoryCounts(items, value), [items, value]);
  const groups = useMemo(() => groupForGrid(items, categories, value, { tab, search, blockedOnly }), [items, categories, value, tab, search, blockedOnly]);
  const byId = useMemo(() => new Map(items.map((s) => [s.id, s])), [items]);
  const hints = useMemo(() => missingRequirements(items, value), [items, value]);
  const known = new Set(items.map((s) => s.id));
  const knownCats = new Set(categories.map((c) => c.id));
  const unknown = q.data ? [...new Set([...value.blocked, ...value.allowed])].filter((v) => !known.has(v)) : [];
  const unknownCats = q.data ? value.categories.filter((c) => !knownCats.has(c)) : [];
  const total = items.length;
  const blockedTotal = [...counts.values()].reduce((n, c) => n + c.blocked, 0);
  const shown = groups.reduce((n, g) => n + g.services.length, 0);
  const visibleTabs = categories.filter((c) => (counts.get(c.id)?.total ?? 0) > 0);
  return (
    <fieldset className="k-fieldset" disabled={isDisabled} data-testid="service-grid">
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>
        {label}
        <Text type="supporting"> ({blockedTotal} blocked)</Text>
      </legend>
      {q.isLoading ? <Text type="supporting">Loading the catalogue…</Text> : null}
      {q.isError ? <Text color="secondary">Could not load the catalogue: {errorText(q.error)}</Text> : null}
      {items.length ? (
        <VStack gap={3}>
          <TabList value={visibleTabs.some((c) => c.id === tab) ? tab : 'all'} onChange={setTab} role="tablist" size="sm" hasDivider>
            <Tab value="all" label={tabLabel('All', total, blockedTotal)} panelId={panelId} />
            {visibleTabs.map((c) => {
              const n = counts.get(c.id) ?? { total: 0, blocked: 0 };
              return <Tab key={c.id} value={c.id} label={tabLabel(c.name, n.total, n.blocked)} icon={<CategoryIcon icon={c.icon} />} panelId={panelId} />;
            })}
          </TabList>
          <HStack gap={3} vAlign="center" wrap="wrap">
            <TextField label="Search services" isLabelHidden startIcon="search" placeholder="Search by name or domain" value={search} onChange={setSearch} hasClear fieldWidth={320} />
            <ToggleButton label="Blocked only" size="md" isPressed={blockedOnly} onPressedChange={setBlockedOnly} data-testid="blocked-only" />
            <Text type="supporting" aria-live="polite">
              {shown === total ? `${total} services` : `Showing ${shown} of ${total}`}
            </Text>
          </HStack>
          <VStack gap={5} id={panelId} role="tabpanel" aria-label={`${label}: catalogue`}>
            {groups.map((g) => (
              <CategorySection
                key={g.category.id}
                category={g.category}
                services={g.services}
                total={g.total}
                blocked={g.blocked}
                value={value}
                onChange={onChange}
                all={items}
                byId={byId}
                hints={hints.filter((h) => h.service.category === g.category.id)}
                isDisabled={isDisabled}
              />
            ))}
            {groups.length === 0 ? <Text type="supporting">{search ? `No service matches "${search}".` : blockedOnly ? 'Nothing is blocked.' : 'No services.'}</Text> : null}
          </VStack>
        </VStack>
      ) : null}
      {description ? (
        <Text type="supporting" display="block">
          {description}
        </Text>
      ) : null}
      {unknown.length || unknownCats.length ? (
        <VStack gap={2}>
          <Text type="supporting">Not in the catalogue (ignored until it is)</Text>
          <Grid columns={{ minWidth: 132 }} gap={2}>
            {unknownCats.map((c) => (
              <button key={`c-${c}`} type="button" className="k-logo-tile" aria-pressed disabled={isDisabled} onClick={() => onChange({ ...value, categories: value.categories.filter((x) => x !== c) })}>
                <CategoryIcon />
                <span className="k-logo-name">{c}</span>
                <span className="k-logo-meta k-error-text">unknown category</span>
              </button>
            ))}
            {unknown.map((id) => (
              <button
                key={id}
                type="button"
                className="k-logo-tile"
                aria-pressed={value.blocked.includes(id)}
                disabled={isDisabled}
                onClick={() => onChange({ ...value, blocked: value.blocked.filter((x) => x !== id), allowed: value.allowed.filter((x) => x !== id) })}
              >
                <ServiceIcon id={id} name={id} />
                <span className="k-logo-name">{id}</span>
                <span className="k-logo-meta k-error-text">{value.blocked.includes(id) ? 'unknown' : 'unknown exception'}</span>
              </button>
            ))}
          </Grid>
        </VStack>
      ) : null}
    </fieldset>
  );
}

interface CategorySectionProps {
  category: CatalogueCategory;
  services: CatalogueService[];
  total: number;
  blocked: number;
  value: ServiceSelection;
  onChange: (sel: ServiceSelection) => void;
  all: CatalogueService[];
  byId: Map<string, CatalogueService>;
  hints: { service: CatalogueService; missing: CatalogueService[] }[];
  isDisabled?: boolean | undefined;
}

function CategorySection({ category: c, services, total, blocked, value, onChange, all, byId, hints, isDisabled }: CategorySectionProps) {
  const headingId = useId();
  const whole = value.categories.includes(c.id);
  return (
    <section aria-labelledby={headingId} data-testid={`category-${c.id}`} data-category={c.id}>
      <VStack gap={2}>
        <HStack gap={3} vAlign="center" hAlign="between" wrap="wrap">
          <HStack gap={2} vAlign="center">
            <CategoryIcon icon={c.icon} />
            <Heading level={3} id={headingId}>
              {c.name}
            </Heading>
            <Text type="supporting">
              {total} service{total === 1 ? '' : 's'}
              {blocked ? ` · ${blocked} blocked` : ''}
            </Text>
          </HStack>
          <SwitchField label="Block the whole category" size="sm" value={whole} isDisabled={isDisabled} onChange={(on) => onChange(setCategory(c.id, on, all, value))} />
        </HStack>
        {c.description || whole ? (
          <Text type="supporting" display="block">
            {whole ? `Every ${c.name} service is blocked, including services a catalogue update adds to the category. Click a service to allow it as an exception.` : c.description}
          </Text>
        ) : null}
        <Grid columns={{ minWidth: 132 }} gap={2}>
          {services.map((s) => (
            <ServiceTile key={s.id} s={s} state={serviceState(s, value)} byId={byId} isDisabled={isDisabled} onToggle={() => onChange(toggleService(s, value))} />
          ))}
        </Grid>
        {hints.map(({ service, missing }) => (
          <HStack key={service.id} gap={2} vAlign="center" wrap="wrap" data-testid={`requires-${service.id}`}>
            <Text type="supporting">
              {service.name} also needs {missing.map((m) => m.name).join(' and ')}: block {missing.length === 1 ? 'it' : 'them'} too to stop {service.name}.
            </Text>
            {missing.map((m) => (
              <Button key={m.id} label={`Block ${m.name}`} size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => onChange(toggleService(m, value))} />
            ))}
          </HStack>
        ))}
      </VStack>
    </section>
  );
}

function tileTitle(s: CatalogueService, byId: Map<string, CatalogueService>): string {
  const names = (ids: string[] | undefined) => (ids ?? []).map((id) => byId.get(id)?.name ?? id).join(', ');
  const parts = [`${s.name}: ${s.rules} rule${s.rules === 1 ? '' : 's'} (${s.domains.slice(0, 3).join(', ')}${s.domains.length > 3 ? ', …' : ''})`];
  if (s.requires?.length) parts.push(`Also needs ${names(s.requires)}.`);
  if (s.related?.length) parts.push(`Related: ${names(s.related)}.`);
  if (s.note) parts.push(s.note);
  if (s.origin === 'adguard') parts.push(`From AdGuard's HostlistsRegistry (${s.license ?? 'GPL-3.0'}), imported by this server.`);
  return parts.join('\n');
}

function ServiceTile({ s, state, byId, isDisabled, onToggle }: { s: CatalogueService; state: ServiceState; byId: Map<string, CatalogueService>; isDisabled?: boolean | undefined; onToggle: () => void }) {
  const on = isBlocked(state);
  const needs = s.requires?.map((id) => byId.get(id)?.name ?? id);
  return (
    <button type="button" className="k-logo-tile" aria-pressed={on} data-state={state} data-service={s.id} disabled={isDisabled} onClick={onToggle} title={tileTitle(s, byId)}>
      <ServiceIcon id={s.id} name={s.name} icon={s.icon} iconOnDark={s.iconOnDark} />
      <span className="k-logo-name">{s.name}</span>
      <span className="k-logo-meta">{STATE_TEXT[state] || (needs?.length ? `Needs ${needs.join(', ')}` : `${s.rules} rule${s.rules === 1 ? '' : 's'}`)}</span>
      {s.origin === 'adguard' ? <Token size="sm" color="gray" label={`AdGuard · ${s.license ?? 'GPL-3.0'}`} description="Imported from AdGuard's HostlistsRegistry" /> : null}
    </button>
  );
}

/** The engines safe search rewrites, with their logos. */
export function EngineList({ engines, mode }: { engines: (Pick<SafeSearchEngine, 'id' | 'name' | 'icon' | 'iconOnDark'> & Partial<Pick<SafeSearchEngine, 'target' | 'moderateTarget'>>)[]; mode?: string }) {
  return (
    <ul className="k-logo-list" aria-label="Safe-search engines">
      {engines.map((e) => (
        <li key={e.id}>
          <ServiceIcon id={e.id} name={e.name} icon={e.icon} iconOnDark={e.iconOnDark} size={24} />
          <span>{e.name}</span>
          {mode === 'moderate' && e.moderateTarget ? <span className="k-muted k-small">moderate</span> : e.target ? <span className="k-muted k-small">strict</span> : null}
        </li>
      ))}
    </ul>
  );
}
