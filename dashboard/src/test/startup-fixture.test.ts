import { DatabaseSync } from 'node:sqlite';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { afterEach, beforeEach, describe, expect, test } from 'vitest';
import { startupRoutesSettled } from '../../e2e/support/startup';

interface FixtureObject {
  spec?: { type?: string; zone?: { uid: string }; source?: { uid: string; resourceVersion: string }; phase?: string; lifecycle?: string; notifySequence?: number; notifySerial?: number; zoneRef?: { name: string } };
  status?: { observedGeneration?: number; conditions?: { type: string; status: string; observedGeneration: number }[] };
}
const ready = () => ({ observedGeneration: 1, conditions: [{ type: 'Ready', status: 'True', observedGeneration: 1 }] });

describe('fresh fixture startup metadata barrier', () => {
  let dir: string, database: string, db: DatabaseSync;
  const insert = (kind: string, name: string, uid: string, data: FixtureObject) => {
    db.prepare('INSERT INTO objects VALUES (?, ?, ?, 1, 7, ?)').run(kind, name, uid, JSON.stringify(data));
  };
  const patch = (kind: string, change: (data: FixtureObject) => void) => {
    const row = db.prepare('SELECT data FROM objects WHERE kind = ?').get(kind)!;
    const data = JSON.parse(String(row.data)) as FixtureObject;
    change(data);
    db.prepare('UPDATE objects SET data = ? WHERE kind = ?').run(JSON.stringify(data), kind);
  };
  const remove = (kind: string) => { db.prepare('DELETE FROM objects WHERE kind = ?').run(kind); };
  beforeEach(() => {
    dir = fs.mkdtempSync(path.join(os.tmpdir(), 'startup-fixture-'));
    database = path.join(dir, 'state.db');
    db = new DatabaseSync(database);
    db.exec('CREATE TABLE objects (kind TEXT, name TEXT, uid TEXT, generation INTEGER, resource_version INTEGER, data TEXT)');
    insert('DNSZone', 'lan.example', 'zone-uid', { spec: { type: 'primary' }, status: ready() });
    insert('ZoneLifecycle', 'lifecycle', 'lifecycle-uid', { spec: { zone: { uid: 'zone-uid' }, source: { uid: 'zone-uid', resourceVersion: '7' }, phase: 'active' } });
    insert('ZoneState', 'head', 'head-uid', { spec: { zone: { uid: 'zone-uid' }, lifecycle: 'active' } });
    insert('DNSRecordSet', 'hosts', 'records-uid', { spec: { zoneRef: { name: 'lan.example' } }, status: ready() });
  });
  afterEach(() => { db.close(); fs.rmSync(dir, { recursive: true, force: true }); });

  test('accepts settled fixed intent without mutating the database', () => {
    const before = fs.readFileSync(database);
    expect(startupRoutesSettled(database, 'lan.example')).toBe(true);
    expect(fs.readFileSync(database)).toEqual(before);
  });

  for (const [name, mutate] of [
    ['missing seeded zone', () => remove('DNSZone')],
    ['missing zone status', () => patch('DNSZone', (data) => { delete data.status; })],
    ['old zone status', () => patch('DNSZone', (data) => { data.status!.observedGeneration = 0; })],
    ['old zone Ready condition', () => patch('DNSZone', (data) => { data.status!.conditions![0]!.observedGeneration = 0; })],
    ['missing lifecycle', () => remove('ZoneLifecycle')],
    ['old lifecycle revision', () => patch('ZoneLifecycle', (data) => { data.spec!.source!.resourceVersion = '6'; })],
    ['wrong lifecycle source UID', () => patch('ZoneLifecycle', (data) => { data.spec!.source!.uid = 'old-uid'; })],
    ['wrong lifecycle zone UID', () => patch('ZoneLifecycle', (data) => { data.spec!.zone!.uid = 'old-uid'; })],
    ['preparing lifecycle', () => patch('ZoneLifecycle', (data) => { data.spec!.phase = 'preparing'; })],
    ['missing head', () => remove('ZoneState')],
    ['old head UID', () => patch('ZoneState', (data) => { data.spec!.zone!.uid = 'old-uid'; })],
    ['tombstoned head', () => patch('ZoneState', (data) => { data.spec!.lifecycle = 'tombstoned'; })],
    ['pending NOTIFY sequence', () => patch('ZoneState', (data) => { data.spec!.notifySequence = 1; })],
    ['pending NOTIFY serial', () => patch('ZoneState', (data) => { data.spec!.notifySerial = 1; })],
    ['missing seeded record', () => remove('DNSRecordSet')],
    ['missing record status', () => patch('DNSRecordSet', (data) => { delete data.status; })],
    ['old record status', () => patch('DNSRecordSet', (data) => { data.status!.observedGeneration = 0; })],
    ['old record Ready condition', () => patch('DNSRecordSet', (data) => { data.status!.conditions![0]!.observedGeneration = 0; })],
  ] as const) {
    test(`rejects ops-ready routes with ${name}`, () => {
      mutate();
      expect(startupRoutesSettled(database, 'lan.example')).toBe(false);
    });
  }

  test('also waits for the bootstrap forward route, without demanding its primary head', () => {
    insert('DNSZone', 'bootstrap-forward', 'forward-uid', { spec: { type: 'forward' } });
    expect(startupRoutesSettled(database, 'lan.example')).toBe(false);
    db.prepare('UPDATE objects SET data = ? WHERE name = ?').run(JSON.stringify({ spec: { type: 'forward' }, status: ready() }), 'bootstrap-forward');
    insert('ZoneLifecycle', 'forward-lifecycle', 'forward-lifecycle-uid', { spec: { zone: { uid: 'forward-uid' }, source: { uid: 'forward-uid', resourceVersion: '7' }, phase: 'active' } });
    expect(startupRoutesSettled(database, 'lan.example')).toBe(true);
  });

  test('permits status-only record revisions without requiring a fabricated new receipt RV', () => {
    db.prepare("UPDATE objects SET resource_version = 99 WHERE kind = 'DNSRecordSet'").run();
    expect(startupRoutesSettled(database, 'lan.example')).toBe(true);
  });
});
