// /audit (audit.read): the append-only audit trail, newest first, filtered
// by action, target and actor (the filters live in the URL, so the license
// and user pages can link to their events), with "Load older events"
// following the keyset cursor.
import { HStack } from '@astryxdesign/core/Stack';
import { AUDIT_TARGET_TYPES, type AuditAction, type AuditQuery, type AuditTargetType } from '@shared/api';
import { AuditTable } from '../components/AuditTable';
import { AUDIT_ACTIONS, TARGET_LABEL } from '../lib/words';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { SelectField } from '../ui/fields/SelectField';
import { TextField } from '../ui/fields/TextField';
import { useSearchParams } from '../ui/lib/router';

const TARGETS: readonly AuditTargetType[] = AUDIT_TARGET_TYPES;

export default function AuditPage() {
  const [sp, setParams] = useSearchParams();
  const action = (sp.get('action') ?? '') as AuditAction | '';
  const targetType = (sp.get('targetType') ?? '') as AuditTargetType | '';
  const targetId = sp.get('targetId') ?? '';
  const actorId = sp.get('actorId') ?? '';
  const query: Omit<AuditQuery, 'before'> = {
    ...(action ? { action } : {}),
    ...(targetType ? { targetType } : {}),
    ...(targetId.trim() ? { targetId: targetId.trim() } : {}),
    ...(actorId.trim() ? { actorId: actorId.trim() } : {}),
    limit: 50,
  };
  const filtered = Boolean(action || targetType || targetId || actorId);
  return (
    <Page>
      <PageHeader title="Audit" description="Every change and every key handed out, as recorded at the time. The trail cannot be edited or deleted." />
      <Panel title="Events">
        <HStack gap={2} wrap="wrap" vAlign="start">
          <SelectField label="Action" value={action} emptyLabel="Any action" options={AUDIT_ACTIONS.map((a) => ({ value: a, label: a }))} onChange={(v) => setParams({ action: v })} width={240} />
          <SelectField label="Target" value={targetType} emptyLabel="Any target" options={TARGETS.map((t) => ({ value: t, label: TARGET_LABEL[t] }))} onChange={(v) => setParams({ targetType: v })} width={200} />
          <TextField label="Target ID" value={targetId} onChange={(v) => setParams({ targetId: v })} hasClear placeholder="lic_…, cus_…, usr_…" fieldWidth={220} />
          <TextField label="Actor ID" value={actorId} onChange={(v) => setParams({ actorId: v })} hasClear placeholder="usr_…" fieldWidth={200} />
        </HStack>
        <AuditTable query={query} empty={filtered ? 'No event matches these filters.' : 'No audit events yet.'} />
      </Panel>
    </Page>
  );
}
