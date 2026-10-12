// Every route's handler, by RouteId: the platform maps (health.ts,
// auth.ts) and the domain map below. main.ts asserts the result covers
// every route of shared/routes.ts before it serves.

import { mergeHandlers } from '../http/router';
import type { PartialHandlerMap } from '../http/types';
import { authHandlers } from './auth';
import { healthHandlers } from './health';

// TODO(stream B): the licensing domain (catalog, me*, customers*, members*,
// licenses*, requests*, blocked emails, the public form) and the admin pages
// (users*, audit, system).
export const domainHandlers: PartialHandlerMap = {};

/** All handlers; a route handled in two maps throws at import. */
export const handlers: PartialHandlerMap = mergeHandlers(healthHandlers, authHandlers, domainHandlers);
