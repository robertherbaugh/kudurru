// The route table of the portal (design spec §7): every page, who may open
// it, and its navigation entry. Pages are separate chunks (React.lazy), so
// the sign-in and confirmation pages load without the back office. Access
// mirrors the API routes each page calls (shared/routes.ts): 'anonymous'
// pages render outside the session gate (/login, /verify, /approve: the
// token in their link is the credential), 'session' pages need a sign-in
// (/invites/:token), and a Permission is checked against Me.permissions
// inside the shell.
import { lazy, type ComponentType, type LazyExoticComponent } from 'react';
import { Building2, Download, FileSignature, Inbox, KeyRound, ScrollText, Server, ShieldCheck, Users, type LucideIcon } from 'lucide-react';
import type { Permission } from '@shared/permissions';
import { matchPath } from './ui/lib/router';

export interface PageProps {
  params: Record<string, string>;
}

type PageComponent = LazyExoticComponent<ComponentType<PageProps>> | ComponentType<PageProps>;

export const SECTIONS = ['Licenses', 'Licensing', 'Administration'] as const;
export type Section = (typeof SECTIONS)[number];

export interface RouteDef {
  path: string;
  /** Which navigation entry is highlighted (detail pages share their list's). */
  navKey: string;
  label: string;
  /** Who may open the page. */
  access: 'anonymous' | 'session' | Permission;
  page: PageComponent;
  /** Listed in the navigation under this section (detail pages are not). */
  section?: Section;
  icon?: LucideIcon;
}

const Login = lazy(() => import('./pages/LoginPage'));
const Verify = lazy(() => import('./pages/VerifyPage'));
const Approve = lazy(() => import('./pages/ApprovePage'));
const Invite = lazy(() => import('./pages/InvitePage'));
const Home = lazy(() => import('./pages/HomePage'));
const MyLicenses = lazy(() => import('./pages/MyLicensesPage'));
const MyLicense = lazy(() => import('./pages/MyLicensePage'));
const Downloads = lazy(() => import('./pages/DownloadsPage'));
const Licenses = lazy(() => import('./pages/LicensesPage'));
const License = lazy(() => import('./pages/LicensePage'));
const Customers = lazy(() => import('./pages/CustomersPage'));
const Customer = lazy(() => import('./pages/CustomerPage'));
const Requests = lazy(() => import('./pages/RequestsPage'));
const OrderForms = lazy(() => import('./pages/OrderFormsPage'));
const OrderForm = lazy(() => import('./pages/OrderFormPage'));
const MyOrderForm = lazy(() => import('./pages/MyOrderFormPage'));
const UsersList = lazy(() => import('./pages/UsersPage'));
const UserDetail = lazy(() => import('./pages/UserPage'));
const Audit = lazy(() => import('./pages/AuditPage'));
const System = lazy(() => import('./pages/SystemPage'));

export const ROUTES: RouteDef[] = [
  { path: '/login', navKey: 'login', label: 'Sign in', access: 'anonymous', page: Login },
  { path: '/verify', navKey: 'verify', label: 'Confirm your key request', access: 'anonymous', page: Verify },
  { path: '/approve', navKey: 'approve', label: 'Approve a key request', access: 'anonymous', page: Approve },
  { path: '/invites/:token', navKey: 'invite', label: 'Invite', access: 'session', page: Invite },
  { path: '/', navKey: 'home', label: 'Home', access: 'session', page: Home },
  { path: '/my/licenses', navKey: 'my', label: 'My licenses', access: 'self.read', page: MyLicenses, section: 'Licenses', icon: KeyRound },
  { path: '/my/licenses/:id', navKey: 'my', label: 'License', access: 'self.read', page: MyLicense },
  { path: '/my/orders/:id', navKey: 'my', label: 'Order Form', access: 'self.read', page: MyOrderForm },
  { path: '/downloads', navKey: 'downloads', label: 'Downloads', access: 'session', page: Downloads, section: 'Licenses', icon: Download },
  { path: '/licenses', navKey: 'licenses', label: 'Licenses', access: 'licenses.read', page: Licenses, section: 'Licensing', icon: ShieldCheck },
  { path: '/licenses/:id', navKey: 'licenses', label: 'License', access: 'licenses.read', page: License },
  { path: '/customers', navKey: 'customers', label: 'Customers', access: 'customers.read', page: Customers, section: 'Licensing', icon: Building2 },
  { path: '/customers/:id', navKey: 'customers', label: 'Customer', access: 'customers.read', page: Customer },
  { path: '/requests', navKey: 'requests', label: 'Requests', access: 'requests.read', page: Requests, section: 'Licensing', icon: Inbox },
  { path: '/orders', navKey: 'orders', label: 'Order Forms', access: 'orders.read', page: OrderForms, section: 'Licensing', icon: FileSignature },
  { path: '/orders/:id', navKey: 'orders', label: 'Order Form', access: 'orders.read', page: OrderForm },
  { path: '/users', navKey: 'users', label: 'Users', access: 'users.read', page: UsersList, section: 'Administration', icon: Users },
  { path: '/users/:id', navKey: 'users', label: 'User', access: 'users.read', page: UserDetail },
  { path: '/audit', navKey: 'audit', label: 'Audit', access: 'audit.read', page: Audit, section: 'Administration', icon: ScrollText },
  { path: '/system', navKey: 'system', label: 'System', access: 'system.read', page: System, section: 'Administration', icon: Server },
];

/** The route a path resolves to, with its parameters (null for an unknown path or a malformed escape such as %E0). */
export function resolveRoute(path: string): { route: RouteDef; params: Record<string, string> } | null {
  try {
    for (const route of ROUTES) {
      const params = matchPath(route.path, path);
      if (params) return { route, params };
    }
  } catch {
    // decodeURIComponent refused a parameter: no page has that address.
  }
  return null;
}
