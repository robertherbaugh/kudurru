// The signed-in frame: Astryx AppShell with the side navigation (every
// page reachable, a mobile drawer below the md breakpoint), the top bar
// (node scope, diagnosis badge, appearance, account) and the sidebar
// status block fed by GET /stats/summary (§10 principle 1).
import { AppShell } from '@astryxdesign/core/AppShell';
import { Link as BrandLink } from '@astryxdesign/core/Link';
import { SideNav, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { TopNav } from '@astryxdesign/core/TopNav';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
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
import { LicenseBanner } from './LicenseBanner';
import { UnblockBadge } from './UnblockBadge';
import { Page, PageHeader } from './layout/Page';

function Brand() {
  return (
    <BrandLink href="/" label="kudurru dashboard home">
      <BrandLogo alt="" />
    </BrandLink>
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
                endContent={r.path === '/messages' ? <MessagesBadge compact /> : r.path === '/requests' ? <UnblockBadge /> : undefined}
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
        <HStack gap={2} vAlign="center" wrap="wrap">
          {match?.route.scoped ? <ScopeSelector /> : null}
          <MessagesBadge />
          <AppearanceButton />
          <AccountMenu />
        </HStack>
      }
    />
  );

  let page: ReactNode;
  if (match) {
    const RoutePage = match.route.page;
    page = <RoutePage params={match.params} />;
  } else {
    page = <NotFound />;
  }

  return (
    <AppShell variant="section" sideNav={nav} topNav={top} banner={
        <>
          <DefaultCredentialsBanner />
          <LicenseBanner />
        </>
      } contentPadding={0}>
      <ErrorBoundary key={loc.path}>
        <Suspense fallback={<FullPageSpinner label="Loading page" />}>{page}</Suspense>
      </ErrorBoundary>
    </AppShell>
  );
}

function NotFound() {
  return (
    <Page>
      <PageHeader title="Page not found" />
      <Text as="p">
        There is no page at this address. <Link href="/">Go to the dashboard</Link>.
      </Text>
    </Page>
  );
}
