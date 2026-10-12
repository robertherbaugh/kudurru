// Sign-in routes (design spec §6 "Sign-in"): authConfig, authLogin,
// authCallback, authMe and authLogout. Stream A implements them here with
// server/auth/{oidc,sessions,safeNext,identity,linking}.ts.

import type { PartialHandlerMap } from '../http/types';

// TODO(stream A): authConfig, authLogin, authCallback, authMe, authLogout.
export const authHandlers: PartialHandlerMap = {};
