// The "Zone settings" section of a zone's page: what the new-zone wizard
// sets (primary: name servers, default TTL, SOA, who may query; forward:
// upstream group, DNSSEC validation), shown and edited in place with the
// wizard's own fields. Saving PUTs the DNSZone with If-Match and keeps every
// field the editor does not show (transfers, NOTIFY, ...).
import { Badge } from '@astryxdesign/core/Badge';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { DNSZone } from '../api/types';
import { readOnlyReason, type Ownership } from '../lib/managedBy';
import { QUERY_ACCESS, SOA_DEFAULTS } from '../lib/zones';
import { EditableSection } from './EditableSection';
import { secondsLabel } from './fields/DurationField';
import { applyDraft, draftFromZone, ForwardFields, primaryError, PrimaryFields, type Draft } from './wizards/ZoneWizard';
import { ErrorBanner, useNotify } from './Feedback';

export interface ZoneSettingsSectionProps {
  /** The DNSZone object name. */
  zone: string;
  own: Ownership;
}

export function ZoneSettingsSection({ zone, own }: ZoneSettingsSectionProps) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const obj = useQuery({ queryKey: [...qk.objects('dnszones'), zone], queryFn: () => api.objects.get<DNSZone>('dnszones', zone) });
  if (obj.error) return <ErrorBanner error={obj.error} />;
  const z = obj.data;
  if (!z || (z.spec.type !== 'primary' && z.spec.type !== 'forward')) return null;
  const value = draftFromZone(z.spec);
  const roReason = !own.editable ? readOnlyReason(own) : !can('zones', 'write') ? 'Needs zones.write' : undefined;

  const save = async (d: Draft) => {
    try {
      await api.objects.put('dnszones', { ...z, spec: applyDraft(z.spec, d) });
    } catch (err) {
      if (err instanceof ApiError && err.status === 412) {
        throw new Error('The zone changed since this page loaded; reload and edit again.', { cause: err });
      }
      throw err;
    }
    notify.success(`Saved the settings of ${z.spec.zoneName}`);
    void qc.invalidateQueries({ queryKey: [...qk.objects('dnszones'), zone] });
    void qc.invalidateQueries({ queryKey: qk.zone(zone) });
    void qc.invalidateQueries({ queryKey: qk.zones });
  };

  const secs = (v: string, k: keyof typeof SOA_DEFAULTS) => (v ? secondsLabel(Number(v)) : <Text color="secondary">default ({secondsLabel(SOA_DEFAULTS[k])})</Text>);
  const access = QUERY_ACCESS.find((o) => o.value === value.queryAccess)?.label ?? value.queryAccess;

  return (
    <EditableSection<Draft>
      title="Zone settings"
      description={value.type === 'primary' ? 'Name servers, TTLs and SOA timers, and who may query the zone.' : 'Where queries for the zone are sent.'}
      value={value}
      readOnlyReason={roReason}
      testId="section-zone-settings"
      view={
        value.type === 'primary' ? (
          <MetadataList columns="multi">
            <MetadataListItem label="Name servers">{value.nameservers.length ? value.nameservers.join(', ') : <Text color="secondary">this server</Text>}</MetadataListItem>
            <MetadataListItem label="Default TTL">{secs(value.defaultTTL, 'defaultTTL')}</MetadataListItem>
            <MetadataListItem label="Refresh">{secs(value.refresh, 'refresh')}</MetadataListItem>
            <MetadataListItem label="Retry">{secs(value.retry, 'retry')}</MetadataListItem>
            <MetadataListItem label="Expire">{secs(value.expire, 'expire')}</MetadataListItem>
            <MetadataListItem label="Negative TTL">{secs(value.minimum, 'minimum')}</MetadataListItem>
            <MetadataListItem label="Who may query">
              {access}
              {value.queryACL.length ? <Text color="secondary"> ({value.queryACL.join(', ')})</Text> : null}
            </MetadataListItem>
          </MetadataList>
        ) : (
          <MetadataList columns="multi">
            <MetadataListItem label="Upstream group">
              {value.upstreamGroup ? (
                <Link href={`/upstreams/${encodeURIComponent(value.upstreamGroup)}`} hasUnderline>
                  {value.upstreamGroup}
                </Link>
              ) : (
                <Text color="secondary">default</Text>
              )}
            </MetadataListItem>
            <MetadataListItem label="DNSSEC validation">
              {value.dnssec === '' ? <Text color="secondary">as the upstream group does</Text> : <Badge variant="neutral" label={value.dnssec === 'on' ? 'validate' : 'do not validate'} />}
            </MetadataListItem>
          </MetadataList>
        )
      }
      edit={(draft, setDraft) => {
        const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setDraft({ ...draft, [k]: v });
        return draft.type === 'primary' ? <PrimaryFields d={draft} set={set} /> : <ForwardFields d={draft} set={set} />;
      }}
      validate={(d) => (d.type === 'primary' ? primaryError(d) : null)}
      onSave={save}
    />
  );
}
