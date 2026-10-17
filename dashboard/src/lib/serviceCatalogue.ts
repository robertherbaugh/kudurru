// The blocked-services selection of a policy (D60, categories): which
// services spec.blockedServices, spec.blockedCategories and
// spec.allowedServices block together, the grid's filters and counts, and
// what a click on a tile or a category switch changes. Pure functions,
// shared by the picker (ServiceGrid), the policy page and the wizard
// review; the server computes the same set (internal/policy
// EffectiveServices).
import type { CatalogueCategory, CatalogueService, PolicySpec } from '../api/types';

/** The three spec fields the picker edits. */
export interface ServiceSelection {
  blocked: string[];
  categories: string[];
  allowed: string[];
}

export const EMPTY_SELECTION: ServiceSelection = { blocked: [], categories: [], allowed: [] };

export function selectionOf(spec: PolicySpec): ServiceSelection {
  return { blocked: spec.blockedServices ?? [], categories: spec.blockedCategories ?? [], allowed: spec.allowedServices ?? [] };
}

/** spec with the selection written back (empty fields left out by cleanPolicySpec). */
export function withSelection(spec: PolicySpec, sel: ServiceSelection): PolicySpec {
  return { ...spec, blockedServices: sel.blocked, blockedCategories: sel.categories, allowedServices: sel.allowed };
}

export function sameSelection(a: ServiceSelection, b: ServiceSelection): boolean {
  const same = (x: string[], y: string[]) => x.length === y.length && x.every((v) => y.includes(v));
  return same(a.blocked, b.blocked) && same(a.categories, b.categories) && same(a.allowed, b.allowed);
}

/** Why a service is blocked, or not. */
export type ServiceState = 'blocked' | 'category' | 'exception' | 'off';

export function serviceState(s: Pick<CatalogueService, 'id' | 'category'>, sel: ServiceSelection): ServiceState {
  if (sel.blocked.includes(s.id)) return 'blocked';
  if (sel.categories.includes(s.category)) return sel.allowed.includes(s.id) ? 'exception' : 'category';
  return 'off';
}

export const isBlocked = (st: ServiceState) => st === 'blocked' || st === 'category';

/**
 * The selection after clicking a service: a blocked one is unblocked (an
 * exception when its category is blocked), an unblocked one is blocked
 * (an exception is withdrawn, else it is listed in blockedServices).
 */
export function toggleService(s: Pick<CatalogueService, 'id' | 'category'>, sel: ServiceSelection): ServiceSelection {
  const without = (xs: string[]) => xs.filter((x) => x !== s.id);
  const inCat = sel.categories.includes(s.category);
  switch (serviceState(s, sel)) {
    case 'blocked':
      return { ...sel, blocked: without(sel.blocked), allowed: inCat ? [...without(sel.allowed), s.id] : sel.allowed };
    case 'category':
      return { ...sel, allowed: [...sel.allowed, s.id] };
    case 'exception':
      return { ...sel, allowed: without(sel.allowed) };
    default:
      return { ...sel, blocked: [...sel.blocked, s.id] };
  }
}

/**
 * The selection with category c blocked or not. Either way the exceptions
 * of its services are dropped (they only mean something while it is
 * blocked); services listed one by one stay listed.
 */
export function setCategory(c: string, on: boolean, services: CatalogueService[], sel: ServiceSelection): ServiceSelection {
  const members = new Set(services.filter((s) => s.category === c).map((s) => s.id));
  const allowed = sel.allowed.filter((id) => !members.has(id));
  const categories = sel.categories.filter((x) => x !== c);
  return { ...sel, categories: on ? [...categories, c] : categories, allowed };
}

/** IDs blocked, in the server's order: blockedServices, then the categories' services minus the exceptions. */
export function effectiveServices(sel: ServiceSelection, services: CatalogueService[]): string[] {
  const out = [...new Set(sel.blocked)];
  const except = new Set(sel.allowed);
  for (const c of sel.categories) {
    for (const s of services) if (s.category === c && !except.has(s.id) && !out.includes(s.id)) out.push(s.id);
  }
  return out;
}

/** Categories in display order; a category the service list uses but the category list lacks is appended. */
export function orderedCategories(categories: CatalogueCategory[], services: CatalogueService[]): CatalogueCategory[] {
  const out = [...categories].sort((a, b) => a.order - b.order || a.id.localeCompare(b.id));
  const known = new Set(out.map((c) => c.id));
  for (const s of services) {
    if (!known.has(s.category)) {
      known.add(s.category);
      out.push({ id: s.category, name: titleCase(s.category), order: 10_000, services: 0 });
    }
  }
  return out;
}

export function titleCase(id: string): string {
  const s = id.replace(/-/g, ' ');
  return s.slice(0, 1).toUpperCase() + s.slice(1);
}

/** Does service s match the search text (name, ID or one of its domains)? */
export function matchesSearch(s: CatalogueService, q: string): boolean {
  const f = q.trim().toLowerCase();
  if (!f) return true;
  return s.name.toLowerCase().includes(f) || s.id.includes(f) || s.domains.some((d) => d.toLowerCase().includes(f));
}

export interface GridFilter {
  /** "all" or a category ID. */
  tab: string;
  search: string;
  blockedOnly: boolean;
}

export interface CategoryGroup {
  category: CatalogueCategory;
  /** The services shown (after the filters), sorted by name. */
  services: CatalogueService[];
  /** All services of the category, and how many of them are blocked. */
  total: number;
  blocked: number;
}

/** The grid's sections: categories in order, each with its filtered services; empty sections are left out. */
export function groupForGrid(services: CatalogueService[], categories: CatalogueCategory[], sel: ServiceSelection, f: GridFilter): CategoryGroup[] {
  const out: CategoryGroup[] = [];
  for (const c of orderedCategories(categories, services)) {
    if (f.tab !== 'all' && f.tab !== c.id) continue;
    const all = services.filter((s) => s.category === c.id);
    const shown = all
      .filter((s) => matchesSearch(s, f.search) && (!f.blockedOnly || isBlocked(serviceState(s, sel))))
      .sort((a, b) => a.name.localeCompare(b.name));
    if (!shown.length) continue;
    out.push({ category: c, services: shown, total: all.length, blocked: all.filter((s) => isBlocked(serviceState(s, sel))).length });
  }
  return out;
}

/** Per-category counts for the tabs: services, and how many are blocked. */
export function categoryCounts(services: CatalogueService[], sel: ServiceSelection): Map<string, { total: number; blocked: number }> {
  const m = new Map<string, { total: number; blocked: number }>();
  for (const s of services) {
    const c = m.get(s.category) ?? { total: 0, blocked: 0 };
    c.total++;
    if (isBlocked(serviceState(s, sel))) c.blocked++;
    m.set(s.category, c);
  }
  return m;
}

/** "AI · 19 · 3 blocked" */
export function tabLabel(name: string, total: number, blocked: number): string {
  return `${name} · ${total}${blocked ? ` · ${blocked} blocked` : ''}`;
}

/** Blocked services whose `requires` are not blocked: blocking them alone does not stop them. */
export function missingRequirements(services: CatalogueService[], sel: ServiceSelection): { service: CatalogueService; missing: CatalogueService[] }[] {
  const byId = new Map(services.map((s) => [s.id, s]));
  const out: { service: CatalogueService; missing: CatalogueService[] }[] = [];
  for (const s of services) {
    if (!isBlocked(serviceState(s, sel)) || !s.requires?.length) continue;
    const missing = s.requires.map((id) => byId.get(id)).filter((r): r is CatalogueService => !!r && !isBlocked(serviceState(r, sel)));
    if (missing.length) out.push({ service: s, missing });
  }
  return out;
}
