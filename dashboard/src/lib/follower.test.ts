import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import type { ClusterInfo } from '../api/types';
import { clusterRole, DOCUMENT_FORWARDED, DOCUMENT_LOCAL, FORWARDED_ROUTES, forwardedOperation, needsPrimary, NO_ROLE, PERMISSION_OPERATIONS, permissionNeedsPrimary, primaryFromMessage, primaryOnlyReason, statusRole } from './follower';

interface Permission { resource?: string; resources?: string[]; verb?: string }
interface Operation { operationId?: string; 'x-kudurru-permission'?: Permission | string; 'x-kudurru-follower'?: string }

function openapi(): Record<string, Record<string, Operation>> {
  const doc = JSON.parse(fs.readFileSync(path.resolve(process.cwd(), '../api/openapi.json'), 'utf8')) as { paths: Record<string, Record<string, Operation>> };
  return doc.paths;
}

const SAFE = new Set(['get', 'head', 'options']);

function info(over: Partial<ClusterInfo>): ClusterInfo {
  return { driver: 'sqlite', epoch: 1, node: 'b', nodes: 2, primaryRequired: false, readOnly: false, took_ms: 0, ...over };
}

describe('follower permission table', () => {
  it('equals the mutation-only permissions of the OpenAPI document', () => {
    const reads = new Set<string>();
    const writes = new Map<string, Set<string>>();
    for (const ops of Object.values(openapi())) {
      for (const [method, op] of Object.entries(ops)) {
        const perm = op['x-kudurru-permission'];
        if (!op.operationId || !perm || typeof perm === 'string' || !perm.verb) continue;
        for (const resource of perm.resources ?? (perm.resource ? [perm.resource] : [])) {
          const key = `${resource}.${perm.verb}`;
          if (SAFE.has(method)) reads.add(key);
          else writes.set(key, (writes.get(key) ?? new Set()).add(op.operationId));
        }
      }
    }
    const expected = Object.fromEntries([...writes].filter(([k]) => !reads.has(k)).sort(([a], [b]) => a.localeCompare(b)).map(([k, ops]) => [k, [...ops].sort()]));
    const actual = Object.fromEntries(Object.entries(PERMISSION_OPERATIONS).sort(([a], [b]) => a.localeCompare(b)).map(([k, ops]) => [k, [...ops].sort()]));
    expect(actual).toEqual(expected);
  });

  it('classifies exactly like the document on a follower that sends the document lists', () => {
    const forwarded: string[] = [];
    const local: string[] = [];
    const primary: string[] = [];
    for (const ops of Object.values(openapi())) {
      for (const [method, op] of Object.entries(ops)) {
        if (SAFE.has(method) || !op.operationId) continue;
        const cls = op['x-kudurru-follower'];
        (cls === 'forwarded' ? forwarded : cls === 'local' ? local : primary).push(op.operationId);
      }
    }
    const role = clusterRole(info({ role: 'follower', primary: 'a', primaryRequired: true, forwardedOperations: forwarded, localOperations: local }));
    for (const op of forwarded) expect(needsPrimary(role, op)).toBe(false);
    for (const op of local) expect(needsPrimary(role, op)).toBe(false);
    for (const op of primary) expect(needsPrimary(role, op)).toBe(true);
    // Mixed permissions stay enabled; the primary-only actions under them are
    // gated by operation.
    expect(permissionNeedsPrimary(role, 'blocklists', 'write')).toBe(false);
    expect(needsPrimary(role, 'createAllowlistEntries')).toBe(true);
    for (const p of ['users.write', 'unblockrequests.decide', 'unblockrequests.delete', 'blocklists.refresh', 'zones.transfer', 'node.config', 'tsigkeys.write', 'rolebindings.write']) {
      const [resource = '', verb = ''] = p.split('.');
      expect(permissionNeedsPrimary(role, resource, verb), p).toBe(true);
    }
    for (const p of ['zones.write', 'records.write', 'policies.delete', 'blocking.toggle', 'cache.flush', 'dhcp.write', 'cluster.promote']) {
      const [resource = '', verb = ''] = p.split('.');
      expect(permissionNeedsPrimary(role, resource, verb), p).toBe(false);
    }
    // A permission that also gates a read is never refused as a whole.
    expect(permissionNeedsPrimary(role, 'settings', 'write')).toBe(false);
    expect(permissionNeedsPrimary(role, 'integrations', 'read')).toBe(false);
  });

  it('keeps the routes of the forwarded operations equal to the document, and matches requests by them', () => {
    const routes: Record<string, string> = {};
    for (const [p, ops] of Object.entries(openapi())) {
      for (const [method, op] of Object.entries(ops)) {
        if (op.operationId && op['x-kudurru-follower'] === 'forwarded') routes[op.operationId] = `${method.toUpperCase()} ${p}`;
      }
    }
    expect({ ...FORWARDED_ROUTES }).toEqual(routes);
    expect(Object.keys(FORWARDED_ROUTES).sort()).toEqual([...DOCUMENT_FORWARDED].sort());
    expect(forwardedOperation('PUT', '/objects/policies/default/kids')).toBe('putObject');
    expect(forwardedOperation('PUT', '/objects/upstreamgroups/default')).toBe('putClusterObject');
    expect(forwardedOperation('POST', '/policies:batchDelete')).toBe('batchDeletePolicies');
    expect(forwardedOperation('DELETE', '/zones/example.org/records/www/A')).toBe('deleteZoneRecord');
    expect(forwardedOperation('POST', '/auth/logout/browser')).toBe('browserLogout');
    expect(forwardedOperation('POST', '/apply?dryRun=true')).toBe('apply');
    expect(forwardedOperation('post', '/dns/blocking')).toBe('setBlocking');
    for (const [method, p] of [['POST', '/dns/query'], ['POST', '/cache/flush'], ['GET', '/settings'], ['POST', '/zones/example.org/notify'], ['DELETE', '/zones//records/www/A'], ['POST', '/objects/policies/x']]) {
      expect(forwardedOperation(method!, p!), `${method} ${p}`).toBeUndefined();
    }
  });

  it('keeps the build-time follower lists equal to the document, for sessions without cluster.read', () => {
    const forwarded: string[] = [];
    const local: string[] = [];
    for (const ops of Object.values(openapi())) {
      for (const [method, op] of Object.entries(ops)) {
        if (SAFE.has(method) || !op.operationId) continue;
        if (op['x-kudurru-follower'] === 'forwarded') forwarded.push(op.operationId);
        if (op['x-kudurru-follower'] === 'local') local.push(op.operationId);
      }
    }
    expect([...DOCUMENT_FORWARDED].sort()).toEqual(forwarded.sort());
    expect([...DOCUMENT_LOCAL].sort()).toEqual(local.sort());
    const fromStatus = statusRole({ localLogin: true, providers: [], hasDefaultCredentials: false, role: 'follower', primary: 'a' });
    const fromCluster = clusterRole(info({ role: 'follower', primary: 'a', primaryRequired: true, forwardedOperations: forwarded, localOperations: local }));
    expect(fromStatus).toEqual(fromCluster);
    expect(statusRole({ localLogin: true, providers: [], hasDefaultCredentials: false })).toBe(NO_ROLE);
    expect(statusRole(undefined)).toBe(NO_ROLE);
  });

  it('gates nothing on a primary, a standalone node or an unknown role', () => {
    for (const role of [NO_ROLE, clusterRole(info({ role: 'primary', primary: 'a' })), clusterRole(info({ role: 'standalone' }))]) {
      expect(role.follower).toBe(false);
      expect(needsPrimary(role, 'createAllowlistEntries')).toBe(false);
      expect(permissionNeedsPrimary(role, 'users', 'write')).toBe(false);
    }
  });

  it('names the primary in the reason and reads it from a refusal', () => {
    expect(primaryOnlyReason('kudurru-0')).toContain('kudurru-0');
    expect(primaryOnlyReason(undefined)).not.toContain('undefined');
    expect(primaryFromMessage('this request runs only on the primary; this node is an enrolled follower of kudurru-0 and does not forward it')).toBe('kudurru-0');
    expect(primaryFromMessage('this request runs only on the primary; this node is an enrolled follower and does not forward it')).toBeUndefined();
  });
});
