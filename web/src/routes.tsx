// The route table: every page, its navigation entry and whether it is a
// stats/log page that carries the node scope control (§10 principle 9).
// Pages are separate chunks (React.lazy) so the initial bundle holds only
// the shell; the dashboard's chart libraries load with the dashboard.
import { lazy, type ComponentType, type LazyExoticComponent } from 'react';
import {
  Activity,
  BookLock,
  Gauge,
  Globe,
  History,
  ListChecks,
  Monitor,
  Network,
  Scale,
  ScrollText,
  Server,
  Settings,
  Siren,
  Users,
  Wrench,
  type LucideIcon,
} from 'lucide-react';
import { matchPath } from './lib/router';

export interface PageProps {
  params: Record<string, string>;
}

type Page = LazyExoticComponent<ComponentType<PageProps>> | ComponentType<PageProps>;

export interface RouteDef {
  path: string;
  /** Which navigation entry is highlighted (detail pages share their list's). */
  navKey: string;
  label: string;
  section: Section;
  icon: LucideIcon;
  page: Page;
  /** Stats/log pages show the node selector. */
  scoped?: boolean;
  /** Not listed in the navigation (detail pages). */
  hidden?: boolean;
}

export const SECTIONS = ['Overview', 'Filtering', 'DNS', 'System'] as const;
export type Section = (typeof SECTIONS)[number];

const Dashboard = lazy(() => import('./pages/DashboardPage'));
const QueryLog = lazy(() => import('./pages/QueryLogPage'));
const LongTerm = lazy(() => import('./pages/LongTermPage'));
const Lists = lazy(() => import('./pages/ListsPage'));
const Groups = lazy(() => import('./pages/GroupsPage'));
const Clients = lazy(() => import('./pages/ClientsPage'));
const Policies = lazy(() => import('./pages/PoliciesPage'));
const Zones = lazy(() => import('./pages/ZonesPage'));
const ZoneDetail = lazy(() => import('./pages/ZoneDetailPage'));
const Upstreams = lazy(() => import('./pages/UpstreamsPage'));
const Nodes = lazy(() => import('./pages/NodesPage'));
const SettingsPage = lazy(() => import('./pages/SettingsPage'));
const Tools = lazy(() => import('./pages/ToolsPage'));
const Messages = lazy(() => import('./pages/MessagesPage'));
const Access = lazy(() => import('./pages/AccessPage'));

export const ROUTES: RouteDef[] = [
  { path: '/', navKey: 'dashboard', label: 'Dashboard', section: 'Overview', icon: Gauge, page: Dashboard, scoped: true },
  { path: '/queries', navKey: 'queries', label: 'Query log', section: 'Overview', icon: Activity, page: QueryLog, scoped: true },
  { path: '/history', navKey: 'history', label: 'Long-term', section: 'Overview', icon: History, page: LongTerm, scoped: true },
  { path: '/lists', navKey: 'lists', label: 'Lists', section: 'Filtering', icon: ListChecks, page: Lists },
  { path: '/groups', navKey: 'groups', label: 'Groups', section: 'Filtering', icon: Users, page: Groups },
  { path: '/clients', navKey: 'clients', label: 'Clients', section: 'Filtering', icon: Monitor, page: Clients, scoped: true },
  { path: '/policies', navKey: 'policies', label: 'Policies', section: 'Filtering', icon: Scale, page: Policies },
  { path: '/zones', navKey: 'zones', label: 'Zones', section: 'DNS', icon: Globe, page: Zones },
  { path: '/zones/:zone', navKey: 'zones', label: 'Zone', section: 'DNS', icon: Globe, page: ZoneDetail, hidden: true },
  { path: '/upstreams', navKey: 'upstreams', label: 'Upstreams', section: 'DNS', icon: Network, page: Upstreams },
  { path: '/nodes', navKey: 'nodes', label: 'Nodes', section: 'System', icon: Server, page: Nodes },
  { path: '/settings', navKey: 'settings', label: 'Settings', section: 'System', icon: Settings, page: SettingsPage },
  { path: '/tools', navKey: 'tools', label: 'Tools', section: 'System', icon: Wrench, page: Tools },
  { path: '/messages', navKey: 'messages', label: 'Diagnosis', section: 'System', icon: Siren, page: Messages },
  { path: '/access', navKey: 'access', label: 'Access & audit', section: 'System', icon: BookLock, page: Access },
];

/** Icons reused by pages for the same concepts. */
export const ICONS = { log: ScrollText };

/** The route a path resolves to, with its parameters. */
export function resolveRoute(path: string): { route: RouteDef; params: Record<string, string> } | null {
  for (const route of ROUTES) {
    const params = matchPath(route.path, path);
    if (params) return { route, params };
  }
  return null;
}
