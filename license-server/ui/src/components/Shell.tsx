// The signed-in frame, after web/src/components/Shell.tsx: Astryx AppShell
// with the side navigation (sections and entries by Me.permissions, a
// mobile drawer below the md breakpoint), the top bar (the product name and
// the account menu) and the banners above every page (a failed session
// check, dev mode, signing unavailable). Each page is checked against its
// route's permission, so a typed URL shows "not permitted" rather than a
// page of failed requests.
import { AppShell } from '@astryxdesign/core/AppShell';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { SideNav, SideNavItem, SideNavSection } from '@astryxdesign/core/SideNav';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { TopNav } from '@astryxdesign/core/TopNav';
import { Suspense, type ReactNode } from 'react';
import { useCan, useSession } from '../api/context';
import { useCatalog, useOpenRequestCount } from '../api/hooks';
import { DEV_BANNER, PRODUCT_NAME } from '../lib/words';
import { resolveRoute, ROUTES, SECTIONS } from '../routes';
import { BrandLogo } from '../ui/components/BrandLogo';
import { ErrorBoundary } from '../ui/components/ErrorBoundary';
import { FullPageSpinner } from '../ui/components/Feedback';
import { Page, PageHeader } from '../ui/components/layout/Page';
import { Link as RouterLink, useLocation } from '../ui/lib/router';
import { AccountMenu } from './AccountMenu';

function Brand() {
  return (
    <Link href="/" label={`${PRODUCT_NAME} home`}>
      <HStack gap={2} vAlign="center">
        <BrandLogo alt="" />
        <Text weight="semibold">license portal</Text>
      </HStack>
    </Link>
  );
}

function RequestsBadge() {
  const q = useOpenRequestCount();
  const n = q.data?.items.length ?? 0;
  return n > 0 ? <Badge variant="info" label={String(n)} /> : null;
}

/** Above every page: a failed session check, dev mode (a test signer), and for issuers a signer that is missing. */
function ShellBanners() {
  const can = useCan();
  const session = useSession();
  const catalog = useCatalog();
  const c = catalog.data;
  return (
    <>
      {session.isError ? (
        <Banner
          status="warning"
          container="section"
          title="The license portal could not be reached"
          description="This page keeps what it last loaded. Changes may fail until the portal answers again."
          endContent={<Button label="Try again" size="sm" variant="secondary" isLoading={session.isFetching} onClick={() => void session.refetch()} />}
        />
      ) : null}
      {c?.signing.testKey ? <Banner status="warning" container="section" title={DEV_BANNER} /> : null}
      {c && !c.signing.available && can('licenses.issue') ? (
        <Banner
          status="warning"
          container="section"
          title="No signing key is loaded"
          description="Licenses cannot be issued, renewed or changed until the operator configures the signing key. The System page says why."
        />
      ) : null}
    </>
  );
}

export function Shell() {
  const loc = useLocation();
  const can = useCan();
  const session = useSession();
  const match = resolveRoute(loc.path);

  const nav = (
    <SideNav>
      {SECTIONS.map((section) => {
        const items = ROUTES.filter((r) => r.section === section && r.access !== 'anonymous' && (r.access === 'session' || can(r.access)));
        if (!items.length) return null;
        return (
          <SideNavSection key={section} title={section}>
            {items.map((r) => {
              const Icon = r.icon;
              return (
                <SideNavItem
                  key={r.path}
                  as={RouterLink}
                  href={r.path}
                  label={r.label}
                  icon={Icon ? <Icon size={16} aria-hidden="true" /> : undefined}
                  isSelected={match?.route.navKey === r.navKey}
                  endContent={r.path === '/requests' ? <RequestsBadge /> : undefined}
                />
              );
            })}
          </SideNavSection>
        );
      })}
    </SideNav>
  );

  const top = (
    <TopNav
      label="Toolbar"
      heading={<Brand />}
      endContent={
        <HStack gap={2} vAlign="center">
          {session.data?.devMode ? <Badge variant="warning" label="Development mode" /> : null}
          <AccountMenu />
        </HStack>
      }
    />
  );

  let page: ReactNode;
  if (!match) page = <NotFound />;
  else if (match.route.access !== 'anonymous' && match.route.access !== 'session' && !can(match.route.access)) page = <NotPermitted />;
  else {
    const RoutePage = match.route.page;
    page = <RoutePage params={match.params} />;
  }

  return (
    <AppShell variant="section" sideNav={nav} topNav={top} banner={<ShellBanners />} contentPadding={0}>
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
        There is no page at this address. <Link href="/">Go to the start page</Link>.
      </Text>
    </Page>
  );
}

function NotPermitted() {
  return (
    <Page>
      <PageHeader title="Not permitted" />
      <Text as="p">
        Your roles do not include this page. <Link href="/">Go to the start page</Link>.
      </Text>
    </Page>
  );
}
