// GET /api/v1/system (design spec §7 /system): the read-only System page.
// No secrets: services/system.ts builds it from the configuration and the
// signing state without the seed, the client secret or any URL with
// credentials.

import type { ResponseOf } from '../../shared/api';
import { json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { systemInfo } from '../services/system';

export const systemHandlers: PartialHandlerMap = {
  system: async (ctx) => json<ResponseOf<'system'>>(await systemInfo(ctx.db, ctx.config, ctx.signing, ctx.mailer)),
};
