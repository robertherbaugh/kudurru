// Every route's handler, by RouteId: the platform maps (health.ts,
// auth.ts) and the licensing domain's maps merged into domainHandlers.
// main.ts asserts the result covers every route of shared/routes.ts before
// it serves; mergeHandlers throws at import when a route is handled twice.

import { mergeHandlers } from '../http/router';
import type { PartialHandlerMap } from '../http/types';
import { auditHandlers } from './audit';
import { authHandlers } from './auth';
import { blocklistHandlers } from './blocklist';
import { catalogHandlers } from './catalog';
import { customerHandlers } from './customers';
import { healthHandlers } from './health';
import { licenseHandlers } from './licenses';
import { meHandlers } from './me';
import { publicHandlers } from './public';
import { requestHandlers } from './requests';
import { systemHandlers } from './system';
import { userHandlers } from './users';

/** The licensing domain (catalog, me*, customers*, members*, licenses*, requests*, blocked emails, the public form) and the admin pages (users*, audit, system). */
export const domainHandlers: PartialHandlerMap = mergeHandlers(
  catalogHandlers,
  meHandlers,
  customerHandlers,
  licenseHandlers,
  requestHandlers,
  blocklistHandlers,
  userHandlers,
  auditHandlers,
  systemHandlers,
  publicHandlers,
);

/** All handlers; a route handled in two maps throws at import. */
export const handlers: PartialHandlerMap = mergeHandlers(healthHandlers, authHandlers, domainHandlers);
