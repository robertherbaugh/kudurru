// Health routes (design spec §8): GET /healthz says the process answers;
// GET /readyz says it can serve: the database answers, its migrations are
// exactly this build's, and whether a signing key is loaded (a missing key
// still counts as ready: the server runs and issuing answers 503). The
// image's HEALTHCHECK (server/healthcheck.ts) calls /readyz.

import type { Readyz, ResponseOf } from '../../shared/api';
import { migrationStatus } from '../db/migrate';
import { json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';

export const healthHandlers: PartialHandlerMap = {
  healthz: () => Promise.resolve(json<ResponseOf<'healthz'>>({ ok: true }, { headers: { 'Cache-Control': 'no-store' } })),

  readyz: async (ctx) => {
    let db: Readyz['db'] = 'ok';
    let migrations: Readyz['migrations'] = 'ok';
    try {
      const status = await migrationStatus(ctx.db);
      if (status.unknown.length > 0) migrations = 'unknown_version';
      else if (status.pending.length > 0) migrations = 'pending';
    } catch (e) {
      ctx.log.warn('readyz: database check failed', { err: e });
      db = 'error';
      migrations = 'error';
    }
    const signing: Readyz['signing'] = ctx.signing.signer ? 'ready' : `unavailable: ${ctx.signing.unavailableReason ?? 'no signing key is loaded'}`;
    const ok = db === 'ok' && migrations === 'ok';
    const body: ResponseOf<'readyz'> = { ok, db, migrations, signing };
    return json(body, { status: ok ? 200 : 503, headers: { 'Cache-Control': 'no-store' } });
  },
};
