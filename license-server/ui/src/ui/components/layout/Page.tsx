// Copied from dashboard/src/components/layout/Page.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// The page scaffolding every dashboard page shares, built from Astryx
// layout components only (dashboard/AGENTS.md): the page frame, the page header
// (back link, h1, state row, description, actions) and the panel (a Card
// with a heading row) that groups one widget or one section of an object
// page.
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useId, type ReactNode } from 'react';

/** The widest a page's content gets (structural width, px). */
export const PAGE_MAX_WIDTH = 1600;

export interface PageProps {
  children: ReactNode;
  /** A narrower frame (wizard pages, forms). */
  maxWidth?: number;
  'data-testid'?: string;
}

/** The frame of a routed page: centered column, one gap between its blocks. */
export function Page({ children, maxWidth = PAGE_MAX_WIDTH, ...rest }: PageProps) {
  return (
    <Center axis="horizontal">
      <VStack gap={4} padding={4} width="100%" maxWidth={maxWidth} {...rest}>
        {children}
      </VStack>
    </Center>
  );
}

export interface PageHeaderProps {
  /** The h1. */
  title: ReactNode;
  /** The parent page ("← Groups"). */
  back?: { href: string; label: string };
  /** A row of state under the title (badges, last seen, owner). */
  meta?: ReactNode;
  /** One supporting sentence under the title. */
  description?: ReactNode;
  /** Buttons and controls at the end of the row. */
  actions?: ReactNode;
}

export function PageHeader({ title, back, meta, description, actions }: PageHeaderProps) {
  return (
    <HStack gap={2} hAlign="between" vAlign="center" wrap="wrap">
      <VStack gap={1}>
        {back ? (
          <Text type="supporting">
            <Link href={back.href} color="inherit">
              ← {back.label}
            </Link>
          </Text>
        ) : null}
        <Heading level={1}>{title}</Heading>
        {meta ? (
          <HStack gap={2} vAlign="center" wrap="wrap">
            {meta}
          </HStack>
        ) : null}
        {description ? (
          <Text type="supporting" as="p">
            {description}
          </Text>
        ) : null}
      </VStack>
      {actions ? (
        <HStack gap={2} vAlign="center" wrap="wrap">
          {actions}
        </HStack>
      ) : null}
    </HStack>
  );
}

export interface PanelProps {
  /** The panel heading (h2 by default, drawn at the h3 size). */
  title?: ReactNode;
  /** Heading level for the document outline (default 2). */
  level?: 2 | 3 | 4;
  /** The heading's id (default: generated); the panel is labeled by it. */
  headingId?: string;
  /** Items after the heading on the same row (badges, notes, buttons). */
  actions?: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  /** A region named by the heading (default), or a plain group. */
  isRegion?: boolean;
  padding?: 0 | 2 | 3 | 4;
  'data-testid'?: string;
  'aria-label'?: string;
}

/**
 * One widget (chart, table, top list) or one section of an object page:
 * a Card with a heading row. Named regions, so tests and screen readers
 * find them by their heading.
 */
export function Panel({ title, level = 2, headingId, actions, description, children, isRegion = true, padding, ...rest }: PanelProps) {
  const genId = useId();
  const id = headingId ?? genId;
  const labeled = title !== undefined && !rest['aria-label'];
  return (
    <Card padding={padding} role={isRegion ? 'region' : undefined} aria-labelledby={isRegion && labeled ? id : undefined} {...rest}>
      <VStack gap={2}>
        {title !== undefined || actions ? (
          <HStack gap={2} hAlign="between" vAlign="center" wrap="wrap">
            {title !== undefined ? (
              // Panels sit one level under the page title but are drawn at the h3 size.
              <Heading level={level === 2 ? 3 : level} accessibilityLevel={level} id={id}>
                {title}
              </Heading>
            ) : null}
            {actions ? (
              <HStack gap={2} vAlign="center" wrap="wrap">
                {actions}
              </HStack>
            ) : null}
          </HStack>
        ) : null}
        {description ? (
          <Text type="supporting" as="p">
            {description}
          </Text>
        ) : null}
        {children}
      </VStack>
    </Card>
  );
}
