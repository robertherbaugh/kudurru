import { describe, expect, it } from 'vitest';
import type { ClientGroup, Policy } from '../api/types';
import { createDB } from '../mocks/fixtures';
import {
  cidrMatch,
  derivePolicyState,
  identifyClient,
  nextScheduleChange,
  parseAddr,
  policyBadge,
  scheduleActive,
  scheduleError,
  scheduleSummary,
  selectPolicy,
  sortByPriority,
} from './policies';
import { toYAML } from './yaml';

const API = 'kudurru.io/v1alpha1' as const;
const pol = (name: string, spec: Policy['spec'], enabled = true): Policy => ({
  apiVersion: API,
  kind: 'Policy',
  metadata: { name, namespace: 'default', ...(enabled ? {} : { annotations: { 'kudurru.io/enabled': 'false' } }) },
  spec,
});
const grp = (name: string, clients: ClientGroup['spec']['clients']): ClientGroup => ({ apiVersion: API, kind: 'ClientGroup', metadata: { name, namespace: 'default' }, spec: { clients } });

describe('schedules (internal/policy/schedule.go)', () => {
  const nights = { weekdays: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu'], start: '21:00', end: '07:00', tz: 'America/Chicago' };

  it('crosses midnight: the morning belongs to the day the window started', () => {
    // Monday 2026-10-12 22:00 in Chicago (UTC-5) is 03:00 UTC on Tuesday.
    expect(scheduleActive(nights, new Date('2026-10-13T03:00:00Z'))).toBe(true);
    // Saturday 06:00 Chicago: Friday's window does not exist.
    expect(scheduleActive(nights, new Date('2026-10-17T11:00:00Z'))).toBe(false);
    // Friday 06:00 Chicago: Thursday's window is still on.
    expect(scheduleActive(nights, new Date('2026-10-16T11:00:00Z'))).toBe(true);
    // Monday 12:00 Chicago: off.
    expect(scheduleActive(nights, new Date('2026-10-12T17:00:00Z'))).toBe(false);
  });

  it('treats no weekdays as every day, no tz as UTC and start == end as 24 hours', () => {
    expect(scheduleActive({ start: '09:00', end: '17:00' }, new Date('2026-10-10T10:00:00Z'))).toBe(true);
    expect(scheduleActive({ start: '09:00', end: '17:00' }, new Date('2026-10-10T18:00:00Z'))).toBe(false);
    expect(scheduleActive({ weekdays: ['sat'], start: '08:00', end: '08:00' }, new Date('2026-10-10T23:59:00Z'))).toBe(true);
    expect(scheduleActive(undefined, new Date())).toBe(true);
  });

  it('finds the next change to the minute', () => {
    const next = nextScheduleChange(nights, new Date('2026-10-12T17:00:00Z'));
    expect(next?.toISOString()).toBe('2026-10-13T02:00:00.000Z');
    expect(nextScheduleChange(undefined, new Date())).toBeUndefined();
  });

  it('validates and summarises', () => {
    expect(scheduleError({ weekdays: ['Funday'], start: '21:00', end: '07:00' })).toBe('Unknown weekday "Funday"');
    expect(scheduleError({ start: '25:00', end: '07:00' })).toBe('Start is HH:MM');
    expect(scheduleError({ start: '21:00', end: '07:00', tz: 'Mars/Olympus' })).toBe('Unknown time zone "Mars/Olympus"');
    expect(scheduleError(nights)).toBeNull();
    expect(scheduleSummary(nights)).toBe('Sun Mon Tue Wed Thu 21:00–07:00 (America/Chicago)');
    expect(scheduleSummary({ start: '', end: '' })).toBe('Every day 00:00–24:00 (UTC)');
  });
});

describe('addresses', () => {
  it('parses IPv4, IPv6 and IPv4-mapped addresses', () => {
    expect(parseAddr('192.168.1.10')).toEqual({ family: 4, value: 0xc0a8010an });
    expect(parseAddr('::ffff:192.168.1.10')).toEqual({ family: 4, value: 0xc0a8010an });
    expect(parseAddr('2001:db8::1')?.family).toBe(6);
    expect(parseAddr('kids-tablet')).toBeNull();
  });

  it('matches CIDRs and reports the prefix length', () => {
    expect(cidrMatch('192.168.1.20/30', '192.168.1.22')).toBe(30);
    expect(cidrMatch('192.168.1.20/30', '192.168.1.24')).toBe(-1);
    expect(cidrMatch('0.0.0.0/0', '10.0.0.5')).toBe(0);
    expect(cidrMatch('::/0', '10.0.0.5')).toBe(-1);
    expect(cidrMatch('2001:db8::/32', '2001:db8::1:10')).toBe(32);
    expect(cidrMatch('10.0.0.5', '10.0.0.5')).toBe(32);
  });
});

describe('client identification and policy selection', () => {
  const groups = [
    grp('default', [{ cidr: '0.0.0.0/0' }, { cidr: '::/0' }]),
    grp('lan', [{ cidr: '192.168.0.0/16' }]),
    grp('kids', [{ cidr: '192.168.1.0/24' }, { clientID: 'Tablet' }]),
    grp('nas', [{ ip: '192.168.1.40' }]),
  ];

  it('stops at the first tier with a match and joins every group of it', () => {
    expect(identifyClient('192.168.1.40', groups)).toMatchObject({ groups: ['default/nas'], tier: 'ip' });
    expect(identifyClient('192.168.1.30', groups)).toMatchObject({ groups: ['default/kids'], tier: 'cidr' });
    expect(identifyClient('192.168.7.1', groups)).toMatchObject({ groups: ['default/lan'], tier: 'cidr' });
    expect(identifyClient('10.1.1.1', groups)).toMatchObject({ groups: ['default/default'], tier: 'catchAll' });
    expect(identifyClient('tablet', groups)).toMatchObject({ groups: ['default/kids'], tier: 'clientID' });
    expect(identifyClient('phone', groups)).toMatchObject({ groups: ['default/default'], tier: 'default' });
  });

  it('ranks by priority, skips disabled and scheduled-off policies and falls back to default', () => {
    const now = new Date('2026-10-12T17:00:00Z');
    const policies = [
      pol('default', { clientGroupRefs: ['default'] }),
      pol('kids-strict', { clientGroupRefs: ['kids'], priority: 20 }),
      pol('kids-off', { clientGroupRefs: ['kids'], priority: 50 }, false),
      pol('kids-night', { clientGroupRefs: ['kids'], priority: 30, schedule: { start: '21:00', end: '07:00' } }),
      pol('kids-extra', { clientGroupRefs: ['kids'], priority: 20 }),
    ];
    const s = selectPolicy(['default/kids'], policies, now);
    expect(s.candidates.map((c) => [c.ref, c.off ?? 'active'])).toEqual([
      ['default/kids-off', 'disabled'],
      ['default/kids-night', 'scheduled off'],
      ['default/kids-extra', 'active'],
      ['default/kids-strict', 'active'],
    ]);
    // Equal priorities: namespace/name decides.
    expect(s.winner?.ref).toBe('default/kids-extra');
    expect(s.active).toHaveLength(2);
    expect(s.fallback).toBe(false);

    const fb = selectPolicy(['default/nas'], policies, now);
    expect(fb).toMatchObject({ fallback: true, winner: { ref: 'default/default' } });
  });

  it('derives the state of a policy, shadowing included', () => {
    const db = createDB();
    const now = new Date('2026-10-12T17:00:00Z');
    const ctx = { policies: db.policies, groups: db.groups, lists: db.lists, now };
    const byName = (n: string) => db.policies.find((p) => p.metadata.name === n)!;
    const def = derivePolicyState(byName('default'), ctx);
    expect(def).toMatchObject({ policy: 'default/default', enabled: true, active: true, shadowedBy: ['default/social-media'], priority: 0 });
    expect(policyBadge(def).key).toBe('shadowed');
    expect(def.lists.find((l) => l.ref === 'default/ui-allow')).toMatchObject({ mode: 'allow', exists: true, ready: true, entries: 2, sourcesTotal: 1 });
    expect(policyBadge(derivePolicyState(byName('streaming-off'), ctx)).key).toBe('disabled');
    expect(policyBadge(derivePolicyState(byName('kids-bedtime'), ctx))).toMatchObject({ key: 'scheduled-off', label: 'Scheduled off' });
    expect(policyBadge(derivePolicyState(byName('social-media'), ctx)).key).toBe('active');
    expect(sortByPriority(db.policies).map((p) => p.metadata.name)).toEqual(['streaming-off', 'kids-bedtime', 'iot-strict', 'social-media', 'default']);
  });
});

describe('routes of the policy, group and client pages', () => {
  it('resolves wizards before detail pages and decodes client keys', async () => {
    const { resolveRoute } = await import('../routes');
    expect(resolveRoute('/policies/new')?.route.label).toBe('New policy');
    expect(resolveRoute('/policies/default/kids')).toMatchObject({ route: { label: 'Policy', navKey: 'policies' }, params: { ns: 'default', name: 'kids' } });
    expect(resolveRoute('/groups/new')?.route.label).toBe('New group');
    expect(resolveRoute('/groups/default/kids')?.params).toEqual({ ns: 'default', name: 'kids' });
    expect(resolveRoute(`/clients/${encodeURIComponent('2001:db8::1')}`)).toMatchObject({ route: { navKey: 'clients' }, params: { key: '2001:db8::1' } });
  });
});

describe('toYAML', () => {
  it('emits block YAML and quotes what a plain scalar would misread', () => {
    expect(
      toYAML({
        apiVersion: 'kudurru.io/v1alpha1',
        metadata: { name: 'kids', annotations: { 'kudurru.io/enabled': 'false' } },
        spec: { clients: [{ ip: '192.168.1.30', name: 'tablet' }, { cidr: '10.0.0.0/8' }], priority: 20, tags: [], schedule: { start: '21:00', weekdays: ['Mon', 'Tue'] }, on: true },
      }),
    ).toBe(
      [
        'apiVersion: kudurru.io/v1alpha1',
        'metadata:',
        '  name: kids',
        '  annotations:',
        '    kudurru.io/enabled: "false"',
        'spec:',
        '  clients:',
        '  - ip: "192.168.1.30"',
        '    name: tablet',
        '  - cidr: "10.0.0.0/8"',
        '  priority: 20',
        '  tags: []',
        '  schedule:',
        '    start: "21:00"',
        '    weekdays:',
        '    - Mon',
        '    - Tue',
        '  "on": true',
        '',
      ].join('\n'),
    );
  });
});
