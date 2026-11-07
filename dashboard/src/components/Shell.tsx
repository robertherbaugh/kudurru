// The signed-in frame: Astryx AppShell with the side navigation (every
// page reachable, a mobile drawer below the md breakpoint), the top bar
// (node scope, diagnosis badge, appearance, account) and the sidebar
// status block fed by GET /stats/summary (§10 principle 1).
import { AppShell, useAppShellMobile } from '@astryxdesign/core/AppShell';
import { Link as BrandLink } from '@astryxdesign/core/Link';
import { SideNav, SideNavCollapseButton, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { TopNav } from '@astryxdesign/core/TopNav';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Suspense, useState, type ReactNode } from 'react';
import { ErrorBoundary } from './ErrorBoundary';
import { FullPageSpinner } from './Feedback';
import { ROUTES, SECTIONS, resolveRoute } from '../routes';
import { useLocation, Link } from '../lib/router';
import { ScopeSelector } from './ScopeSelector';
import { MessagesBadge } from './MessagesBadge';
import { AppearanceButton } from './AppearanceButton';
import { AccountMenu } from './AccountMenu';
import { useBranding } from '../lib/branding';
import { BrandLogo } from './BrandLogo';
import { SidebarStatus } from './SidebarStatus';
import { DefaultCredentialsBanner } from './DefaultCredentialsBanner';
import { LicenseBanner } from './LicenseBanner';
import { UnblockBadge } from './UnblockBadge';
import { Page, PageHeader } from './layout/Page';

function Brand() {
  const brand = useBranding();
  return (
    <BrandLink href="/" label={`${brand.name || 'kudurru'} dashboard home`}>
      <BrandLogo alt="" />
    </BrandLink>
  );
}

function Navigation({ navKey }: { navKey?: string }) {
  const { isMobile } = useAppShellMobile();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [isHovered, setIsHovered] = useState(false);
  const [hasFlyoutFocus, setHasFlyoutFocus] = useState(false);
  const isExpanded = isMobile || !isCollapsed || isHovered || hasFlyoutFocus;
  // The button controls the pinned state, even while hover reveals the nav.
  const collapsible = {
    isCollapsed,
    onCollapsedChange: (collapsed: boolean) => {
      setIsCollapsed(collapsed);
      setIsHovered(false);
      setHasFlyoutFocus(false);
    },
  };

  const nav = (
    <SideNav
      className="k-navigation-nav"
      collapsible={isMobile ? false : { isCollapsed: !isExpanded, hasButton: false }}
      footer={
        // Keep the controls mounted so leaving the flyout cannot dismiss a dialog.
        <VStack className="k-navigation-status" hidden={!isExpanded}>
          <SidebarStatus />
        </VStack>
      }
      footerIcons={<SideNavCollapseButton collapsible={collapsible} aria-expanded={!isCollapsed} />}
    >
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
                isSelected={navKey === r.navKey}
                endContent={r.path === '/messages' ? <MessagesBadge compact /> : r.path === '/requests' ? <UnblockBadge /> : undefined}
              />
            );
          })}
        </SideNavSection>
      ))}
    </SideNav>
  );

  if (isMobile) return nav;

  return (
    <VStack
      className="k-navigation"
      height="100%"
      data-collapsed={isCollapsed}
      data-flyout={isCollapsed && isExpanded}
      onPointerEnter={(event) => {
        if (event.pointerType === 'mouse' || event.pointerType === 'pen') setIsHovered(true);
      }}
      onPointerLeave={() => setIsHovered(false)}
      onFocusCapture={(event) => {
        // Keep a revealed flyout open while its controls have keyboard focus.
        if (isHovered && event.target.matches(':focus-visible')) setHasFlyoutFocus(true);
      }}
      onBlurCapture={(event) => {
        if (!event.currentTarget.contains(event.relatedTarget)) setHasFlyoutFocus(false);
      }}
    >
      {nav}
    </VStack>
  );
}

export function Shell() {
  const loc = useLocation();
  const match = resolveRoute(loc.path);

  const top = (
    <TopNav
      label="Toolbar"
      heading={<Brand />}
      endContent={
        <HStack gap={2} vAlign="center" wrap="wrap">
          {match?.route.scoped === true ? <ScopeSelector /> : null}
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
    <AppShell variant="section" sideNav={<Navigation navKey={match?.route.navKey} />} topNav={top} banner={
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
