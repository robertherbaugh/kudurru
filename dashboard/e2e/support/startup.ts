// Fixture-only observation of bootstrap status/lifecycle work. Ops readiness
// permits an admitted route whose status update still queues another publication.
// Only fresh first-incarnation fixture databases with fixed seeded intent apply.
// This is not a production admission promise or a general restore diagnostic.
import { DatabaseSync } from 'node:sqlite';

/** Read only route metadata; never load credential, session or security rows. */
export function startupRoutesSettled(database: string, seededZone: string): boolean {
  const db = new DatabaseSync(database, { readOnly: true });
  try {
    db.exec('BEGIN'); // One read snapshot, with no credential/security tables.
    const row = db.prepare(`
      SELECT
        count(*) AS routes,
        sum(CASE WHEN z.name = ? AND json_extract(z.data, '$.spec.type') = 'primary' THEN 1 ELSE 0 END) AS seeded,
        sum(CASE WHEN
          json_extract(z.data, '$.status.observedGeneration') = z.generation
          AND EXISTS (
            SELECT 1 FROM json_each(z.data, '$.status.conditions') AS condition
            WHERE json_extract(condition.value, '$.type') = 'Ready'
              AND json_extract(condition.value, '$.status') = 'True'
              AND json_extract(condition.value, '$.observedGeneration') = z.generation
          )
          AND EXISTS (
            SELECT 1 FROM objects AS lifecycle
            WHERE lifecycle.kind = 'ZoneLifecycle'
              AND json_extract(lifecycle.data, '$.spec.zone.uid') = z.uid
              AND json_extract(lifecycle.data, '$.spec.source.uid') = z.uid
              AND json_extract(lifecycle.data, '$.spec.source.resourceVersion') = cast(z.resource_version AS TEXT)
              AND json_extract(lifecycle.data, '$.spec.phase') = 'active'
          )
          AND (json_extract(z.data, '$.spec.type') = 'forward' OR EXISTS (
            SELECT 1 FROM objects AS head
            WHERE head.kind = 'ZoneState'
              AND json_extract(head.data, '$.spec.zone.uid') = z.uid
              AND json_extract(head.data, '$.spec.lifecycle') = 'active'
              AND coalesce(json_extract(head.data, '$.spec.notifySequence'), 0) = 0
              AND coalesce(json_extract(head.data, '$.spec.notifySerial'), 0) = 0
          ))
          THEN 1 ELSE 0 END) AS settled
      FROM objects AS z
      WHERE z.kind = 'DNSZone' AND json_extract(z.data, '$.spec.type') IN ('primary', 'forward')
    `).get(seededZone);
    if (row === undefined || row.seeded !== 1 || row.routes !== row.settled) return false;
    const pending = db.prepare(`
      SELECT count(*) AS records,
        sum(CASE WHEN json_extract(record.data, '$.spec.zoneRef.name') = ? THEN 1 ELSE 0 END) AS seeded,
        sum(CASE WHEN
        coalesce(json_extract(record.data, '$.status.observedGeneration'), -1) != record.generation
        OR NOT EXISTS (
          SELECT 1 FROM json_each(record.data, '$.status.conditions') AS condition
          WHERE json_extract(condition.value, '$.type') = 'Ready'
            AND json_extract(condition.value, '$.status') = 'True'
            AND json_extract(condition.value, '$.observedGeneration') = record.generation
        )
        THEN 1 ELSE 0 END) AS pending FROM objects AS record
      WHERE record.kind = 'DNSRecordSet'
    `).get(seededZone);
    return pending?.seeded === 1 && pending.pending === 0;
  } finally {
    db.close();
  }
}
