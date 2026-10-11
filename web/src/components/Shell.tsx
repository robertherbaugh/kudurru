// The signed-in frame: Astryx AppShell with the side navigation (every
// page reachable, a mobile drawer below the md breakpoint), the top bar
// (node scope, diagnosis badge, appearance, account) and the sidebar
// status block fed by GET /stats/summary (§10 principle 1).
import { AppShell } from '@astryxdesign/core/AppShell';
import { SideNav, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { TopNav } from '@astryxdesign/core/TopNav';
import { Suspense, type ReactNode } from 'react';
import { ErrorBoundary } from './ErrorBoundary';
import { FullPageSpinner } from './Feedback';
import { ROUTES, SECTIONS, resolveRoute } from '../routes';
import { useLocation, Link } from '../lib/router';
import { ScopeSelector } from './ScopeSelector';
import { MessagesBadge } from './MessagesBadge';
import { AppearanceButton } from './AppearanceButton';
import { AccountMenu } from './AccountMenu';
import { BrandLogo } from './BrandLogo';
import { SidebarStatus } from './SidebarStatus';
import { DefaultCredentialsBanner } from './DefaultCredentialsBanner';

function Brand() {
  return (
    <Link href="/" className="k-brand" aria-label="kudurru dashboard home">
      <BrandLogo alt="" />
    </Link>
  );
}

export function Shell() {
  const loc = useLocation();
  const match = resolveRoute(loc.path);

  const nav = (
    <SideNav footer={<SidebarStatus />}>
      {SECTIONS.map((section) => (
        <SideNavSection key={section} title={section}>
          {ROUTES.filter((r) => r.section === section && !r.hidden).map((r) => {
            const Icon = r.icon;
            return (
              <SideNavItem
                key={r.path}
                as={Link}
                href={r.path}
                label={r.label}
                icon={<Icon size={16} aria-hidden="true" />}
                isSelected={match?.route.navKey === r.navKey}
                endContent={r.path === '/messages' ? <MessagesBadge compact /> : undefined}
              />
            );
          })}
        </SideNavSection>
      ))}
    </SideNav>
  );

  const top = (
    <TopNav
      label="Toolbar"
      heading={<Brand />}
      endContent={
        <div className="k-row">
          {match?.route.scoped ? <ScopeSelector /> : null}
          <MessagesBadge />
          <AppearanceButton />
          <AccountMenu />
        </div>
      }
    />
  );

  let page: ReactNode;
  if (match) {
    const Page = match.route.page;
    page = <Page params={match.params} />;
  } else {
    page = <NotFound />;
  }

  return (
    <div className="k-app">
      <AppShell variant="section" sideNav={nav} topNav={top} banner={<DefaultCredentialsBanner />} contentPadding={0}>
        <ErrorBoundary key={loc.path}>
          <Suspense fallback={<FullPageSpinner label="Loading page" />}>{page}</Suspense>
        </ErrorBoundary>
      </AppShell>
    </div>
  );
}

function NotFound() {
  return (
    <div className="k-page">
      <h1>Page not found</h1>
      <p>
        There is no page at this address. <Link href="/">Go to the dashboard</Link>.
      </p>
    </div>
  );
}
