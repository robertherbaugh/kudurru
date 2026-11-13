// What an enrolled SQLite follower does with a change (D368). GET /cluster
// reports the node's role, its primary and, on a follower, the operationIds
// that it forwards to the primary or runs itself; every other mutation answers
// 503 primary_required. The dashboard reads the node's own lists, so a newer
// or older follower is described by what it actually does.
import type { AuthStatus, ClusterInfo } from '../api/types';

export interface ClusterRole {
  /** standalone, primary or follower on the SQLite driver; undefined elsewhere. */
  role: string | undefined;
  /** True on an enrolled follower: unlisted mutations need the primary. */
  follower: boolean;
  /** The node that holds write authority, when known. */
  primary: string | undefined;
  forwarded: ReadonlySet<string>;
  local: ReadonlySet<string>;
}

const NONE: ReadonlySet<string> = new Set();

export const NO_ROLE: ClusterRole = { role: undefined, follower: false, primary: undefined, forwarded: NONE, local: NONE };

export function clusterRole(info: ClusterInfo | undefined): ClusterRole {
  if (!info) return NO_ROLE;
  const follower = info.primaryRequired === true;
  return {
    role: info.role || undefined,
    follower,
    primary: info.primary || undefined,
    forwarded: follower ? new Set(info.forwardedOperations ?? []) : NONE,
    local: follower ? new Set(info.localOperations ?? []) : NONE,
  };
}

/**
 * The operationIds an enrolled follower of this release forwards to its
 * primary (`x-kudurru-follower: forwarded` in api/openapi.json). Used only
 * when GET /cluster cannot be read (it needs cluster.read, which only the
 * admin role holds) and the public GET /auth/status says the node is a
 * follower; follower.test.ts keeps it equal to the OpenAPI document.
 */
export const DOCUMENT_FORWARDED: readonly string[] = [
  'apply', 'batchDeleteBlocklists', 'batchDeleteClientgroups', 'batchDeleteDnsrecordsets', 'batchDeleteDnszones', 'batchDeleteIntegrations',
  'batchDeletePolicies',
  'browserLogout', 'changePassword', 'createBlocklists', 'createClientgroups', 'createObject', 'createPolicies', 'createToken',
  'deleteBlocklists', 'deleteClientgroups', 'deleteClusterObject', 'deleteObject', 'deletePolicies', 'deleteToken', 'deleteZone',
  'deleteZoneRecord', 'importZone', 'login', 'logout', 'patchSettings', 'putBlocklists', 'putClientgroups', 'putClusterObject',
  'putObject', 'putPolicies', 'revokeSession', 'setBlocking', 'upsertZoneRecord',
];

/** The operationIds a follower of this release runs itself
 * (`x-kudurru-follower: local`); see DOCUMENT_FORWARDED. */
export const DOCUMENT_LOCAL: readonly string[] = [
  'activateCluster', 'changeSettingsOwnership', 'deleteDHCPReservation', 'dnsQuery', 'exportSettingsSectionYAML', 'flushCache',
  'importDHCPLeases', 'issueClusterJoinToken', 'promoteCluster', 'purgeQuerylog', 'putDHCPReservation', 'putDHCPScopeConfig',
  'putDHCPSetup', 'verifyRollupEvidence',
];

/**
 * The role from the public GET /auth/status, for a session that cannot read
 * GET /cluster: a follower names itself and its primary there, and its lists
 * are taken from the OpenAPI document this dashboard was built with (the
 * dashboard is embedded in the binary, so they describe the same release).
 */
export function statusRole(status: AuthStatus | undefined): ClusterRole {
  if (status?.role !== 'follower') return NO_ROLE;
  return { role: 'follower', follower: true, primary: status.primary || undefined, forwarded: new Set(DOCUMENT_FORWARDED), local: new Set(DOCUMENT_LOCAL) };
}

/**
 * The mutating operationIds of every permission that gates mutations only
 * (`x-kudurru-permission` of api/openapi.json; a permission that also gates a
 * read, such as settings.write's sibling settings.read, is not listed, so a
 * follower never hides data). follower.test.ts keeps this table equal to the
 * OpenAPI document.
 */
export const PERMISSION_OPERATIONS: Readonly<Record<string, readonly string[]>> = {
  'blocking.toggle': ['setBlocking'],
  'blocklists.delete': ['batchDeleteBlocklists', 'deleteBlocklists', 'deleteClusterObject', 'deleteObject'],
  'blocklists.refresh': ['refreshBlockList'],
  'blocklists.write': [
    'addDomains',
    'apply',
    'batchAllowlistEntries',
    'batchDenylistEntries',
    'createAllowlistEntries',
    'createBlocklists',
    'createDenylistEntries',
    'createObject',
    'deleteAllowlistEntry',
    'deleteDenylistEntry',
    'patchAllowlistEntry',
    'patchDenylistEntry',
    'patchSourceEntries',
    'putBlocklists',
    'putClusterObject',
    'putObject',
    'queryAction',
  ],
  'cache.flush': ['flushCache'],
  'clientgroups.delete': ['batchDeleteClientgroups', 'deleteClientgroups', 'deleteClusterObject', 'deleteObject'],
  'clientgroups.write': ['apply', 'createClientgroups', 'createObject', 'putClientgroups', 'putClusterObject', 'putObject'],
  'cluster.join': ['activateCluster', 'issueClusterJoinToken'],
  'cluster.promote': ['promoteCluster'],
  'dhcp.delete': ['batchDeleteDhcpscopes', 'deleteClusterObject', 'deleteDHCPReservation', 'deleteObject'],
  'dhcp.write': ['apply', 'createObject', 'putClusterObject', 'putDHCPReservation', 'putDHCPScopeConfig', 'putDHCPSetup', 'putObject'],
  'dhcpleases.import': ['importDHCPLeases'],
  'edgeblocks.write': ['createEdgeBlock', 'revokeEdgeBlock'],
  'integrations.act': ['invokeIntegrationAction'],
  'integrations.check': ['checkIntegration'],
  'integrations.delete': ['batchDeleteIntegrations', 'deleteClusterObject', 'deleteObject'],
  'integrations.release': ['releaseIntegration'],
  'integrations.sync': ['syncIntegration'],
  'integrations.write': ['apply', 'createObject', 'putClusterObject', 'putObject'],
  'node.config': ['patchNodeConfig'],
  'policies.delete': ['batchDeletePolicies', 'deleteClusterObject', 'deleteObject', 'deletePolicies'],
  'policies.write': ['apply', 'createObject', 'createPolicies', 'putClusterObject', 'putObject', 'putPolicies'],
  'querylog.purge': ['purgeQuerylog'],
  'records.delete': ['batchDeleteDnsrecordsets', 'deleteClusterObject', 'deleteObject', 'deleteZoneRecord'],
  'records.write': ['apply', 'createObject', 'importZone', 'putClusterObject', 'putObject', 'upsertZoneRecord'],
  'rolebindings.delete': ['deleteRoleBinding'],
  'rolebindings.write': ['createRoleBinding', 'replaceRoleBinding'],
  'tsigkeys.delete': ['revokeStoredTSIGKey'],
  'tsigkeys.write': ['createStoredTSIGKey', 'replaceStoredTSIGKey', 'resealStoredTSIGKey'],
  'unblockrequests.decide': ['approveUnblockRequest', 'denyUnblockRequest'],
  'unblockrequests.delete': ['deleteUnblockRequest'],
  'users.write': ['createLocalUser', 'disableExternalUser', 'enableExternalUser', 'logoutExternalUser', 'recoverTOTP', 'resetLocalUserPassword'],
  'zones.delete': ['batchDeleteDnszones', 'deleteClusterObject', 'deleteObject', 'deleteZone'],
  'zones.transfer': ['notifyZone'],
  'zones.write': ['apply', 'createObject', 'putClusterObject', 'putObject'],
};

/** Whether this node answers the operation with 503 primary_required. */
export function needsPrimary(role: ClusterRole, operationId: string): boolean {
  return role.follower && !role.forwarded.has(operationId) && !role.local.has(operationId);
}

/** Whether every operation that resource.verb gates needs the primary. */
export function permissionNeedsPrimary(role: ClusterRole, resource: string, verb: string): boolean {
  if (!role.follower) return false;
  const ops = PERMISSION_OPERATIONS[`${resource}.${verb}`];
  return !!ops && ops.every((op) => needsPrimary(role, op));
}

/** The tooltip of a control that only the primary can carry out. */
export function primaryOnlyReason(primary: string | undefined): string {
  return primary
    ? `Runs only on the primary, ${primary}: this node is a follower and does not forward it`
    : 'Runs only on the primary: this node is a follower and does not forward it';
}

/** The primary named by a primary_required message, when it names one. */
export function primaryFromMessage(message: string): string | undefined {
  return /follower of (\S+) and does not forward/.exec(message)?.[1];
}
