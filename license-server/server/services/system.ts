// The read-only System page and the catalog (design spec §7 /system, §8
// GET /api/v1/system and GET /api/v1/catalog): what the portal runs with,
// from the configuration and the signing state decided at startup, plus
// how many keys each kid signed. No secrets: the signing seed, the client
// secret, SMTP_URL and the database URLs never appear.

import type { Catalog, SystemInfo } from '../../shared/api';
import { FEATURES, PLANS, POLICY } from '../../shared/catalog';
import { todayUtc } from '../../shared/dates';
import type { Config } from '../config';
import { toInt, type Queryable } from '../db/sql';
import { trustedKids, type SigningState } from '../licensing/types';
import type { Mailer } from '../mail/types';

/** The kid's .pub is in the trusted dir and matches the loaded key. */
function signerTrusted(signing: SigningState): boolean {
  const s = signing.signer;
  return s !== null && signing.keyring.get(s.kid) === s.publicKey;
}

/** GET /api/v1/catalog. */
export function catalog(signing: SigningState, mailer: Mailer, now: Date): Catalog {
  const s = signing.signer;
  return {
    plans: [...PLANS],
    features: [...FEATURES],
    policy: { ...POLICY },
    signing: {
      available: s !== null,
      ...(s ? { kid: s.kid } : {}),
      ...(s?.trustedSince ? { trustedSince: s.trustedSince } : {}),
      testKey: s?.testKey ?? false,
    },
    mail: { configured: mailer.configured },
    today: todayUtc(now),
  };
}

/** GET /api/v1/system. */
export async function systemInfo(q: Queryable, config: Config, signing: SigningState, mailer: Mailer): Promise<SystemInfo> {
  const s = signing.signer;
  const counts = await q<{ kid: string; n: string }[]>`select kid, count(*) as n from license_keys group by kid order by kid`;
  return {
    devMode: config.devMode,
    signing: {
      available: s !== null,
      kid: s?.kid ?? null,
      trusted: signerTrusted(signing),
      trustedSince: s?.trustedSince ?? null,
      testKey: s?.testKey ?? false,
      unavailableReason: signing.unavailableReason,
      trustedKids: trustedKids(signing),
      keysByKid: counts.map((c) => ({ kid: c.kid, count: toInt(c.n) })),
    },
    signIn: {
      issuer: config.oidc.issuer,
      clientId: config.oidc.clientId,
      scopes: [...config.oidc.scopes],
      displayName: config.oidc.displayName,
      groupsClaim: config.oidc.groupsClaim,
      idTokenAlg: config.oidc.idTokenAlg,
      roleMappings: {
        customer: [...config.roles.groups.customer],
        license_admin: [...config.roles.groups.license_admin],
        superadmin: [...config.roles.groups.superadmin],
      },
      adminClaimRules: config.oidc.adminClaimRules.map((r) => ({ claim: r.claim, value: r.value })),
      defaultRole: config.roles.defaultRole,
      memberAutoLink: config.roles.memberAutoLink,
    },
    sessions: { idleTimeoutSeconds: Math.round(config.session.idleTimeoutMs / 1000), lifetimeSeconds: Math.round(config.session.lifetimeMs / 1000) },
    mail: { configured: mailer.configured, from: config.mail.from, replyTo: config.mail.replyTo, adminNotify: config.mail.adminNotify },
    publicForm: {
      enabled: config.publicForm.enabled,
      origins: [...config.publicForm.origins],
      limitPerIpPerHour: config.publicForm.limitPerIpPerHour,
      limitPerEmailPerDay: config.publicForm.limitPerEmailPerDay,
      personalAutoApprove: config.personal.autoApprove,
      personalAutoApproveMaxPerDay: config.personal.autoApproveMaxPerDay,
    },
    issuing: { maxPerActorPerDay: config.issuing.maxPerActorPerDay },
    policy: { ...POLICY },
    build: { version: config.build.version, commit: config.build.commit, builtAt: config.build.builtAt },
  };
}
