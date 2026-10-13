// GET /api/v1/catalog (design spec §8): the plans, features and policy the
// UI words and validates with, whether keys can be signed (and with a test
// key), whether mail is configured, and today as the server sees it (UTC).

import type { ResponseOf } from '../../shared/api';
import { json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { catalog } from '../services/system';

export const catalogHandlers: PartialHandlerMap = {
  catalog: (ctx) => Promise.resolve(json<ResponseOf<'catalog'>>(catalog(ctx.signing, ctx.mailer, ctx.now))),
};
