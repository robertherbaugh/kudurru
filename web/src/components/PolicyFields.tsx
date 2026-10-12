// The typed editors of a Policy, one per concept, shared by the
// /policies/new wizard (one step each) and the policy page (one section
// each): Who (client groups, priority), What (block and allow lists,
// allow-only), Blocked services, Safe search, How (block response, block
// TTL, upstream group, query log) and When (schedule). Each takes the spec
// and returns a new one; references are bare names in the policy's
// namespace, as the API stores them.
import { Card } from '@astryxdesign/core/Card';
import { Code } from '@astryxdesign/core/Code';
import { Grid } from '@astryxdesign/core/Grid';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { qk, useApi } from '../api/context';
import type { AnyObject, BlockList, ClientGroup, Policy, PolicySpec } from '../api/types';
import { formatExact } from '../lib/format';
import { BLOCK_RESPONSES, SAFE_SEARCH_MODES, scheduleError, sortByPriority } from '../lib/policies';
import { BLP_LICENCE_NOTE, CONTENT_CATEGORIES, CONTENT_GROUPS, contentListName } from '../lib/contentCategories';
import { durationError } from '../lib/objectSchemas';
import { selectionOf, withSelection } from '../lib/serviceCatalogue';
import { DurationField, type DurationPreset } from './fields/DurationField';
import { EnumSelect } from './fields/EnumSelect';
import { NumberField } from './fields/NumberField';
import { ObjectRefPicker } from './fields/ObjectRefPicker';
import { ScheduleField } from './fields/ScheduleField';
import { CheckboxField, SwitchField } from './fields/TextField';
import { EngineList, ServiceGrid, useSafeSearchEngines } from './ServiceGrid';
import { UpstreamGroupSelect } from './UpstreamGroupSelect';

export interface PolicyFieldsProps {
  spec: PolicySpec;
  onChange: (spec: PolicySpec) => void;
  namespace: string;
  isDisabled?: boolean;
  showErrors?: boolean;
  /** The policy being edited (excluded from "other policies" hints). */
  self?: string;
}

export const PRIORITY_MIN = 0;
export const PRIORITY_MAX = 100000;

/** Drops empty arrays and strings so the stored spec stays minimal. */
export function cleanPolicySpec(spec: PolicySpec): PolicySpec {
  const out: PolicySpec = {};
  for (const [k, v] of Object.entries(spec) as [keyof PolicySpec, unknown][]) {
    if (v === undefined || v === '' || (Array.isArray(v) && v.length === 0)) continue;
    (out as Record<string, unknown>)[k] = v;
  }
  return out;
}

const isList = (mode: 'block' | 'allow') => (o: AnyObject) => ((o as BlockList).spec.mode ?? 'block') === mode;
const listDetail = (o: AnyObject) => {
  const l = o as BlockList;
  return `${formatExact(l.status?.entries ?? 0)} entries · ${l.spec.sources.length} source${l.spec.sources.length === 1 ? '' : 's'}`;
};
const groupDetail = (o: AnyObject) => {
  const g = o as ClientGroup;
  const n = g.spec.clients?.length ?? 0;
  return `${n} matcher${n === 1 ? '' : 's'} · ${formatExact(g.status?.members ?? 0)} seen`;
};
const allowList = isList('allow');
const blockListFilter = isList('block');

/** The fieldset keeps its semantics (legend, disabling every control inside); the Card around it draws the frame. */
const FIELDSET_RESET = { border: 'none', minInlineSize: 0 } as const;

// -- Who ----------------------------------------------------------------------

export function whoError(spec: PolicySpec): string | null {
  if (!spec.clientGroupRefs?.length) return 'Pick at least one client group; a policy without groups applies to nobody.';
  return priorityError(spec.priority);
}

export function priorityError(p: number | undefined): string | null {
  if (p === undefined) return null;
  if (!Number.isInteger(p) || p < PRIORITY_MIN || p > PRIORITY_MAX) return `Priority is a whole number from ${PRIORITY_MIN} to ${PRIORITY_MAX}.`;
  return null;
}

export function WhoFields({ spec, onChange, namespace, isDisabled, showErrors, self }: PolicyFieldsProps) {
  const api = useApi();
  const pols = useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies') });
  const groups = new Set(spec.clientGroupRefs ?? []);
  const others = sortByPriority(pols.data?.items ?? []).filter(
    (p) => `${p.metadata.namespace}/${p.metadata.name}` !== self && (p.metadata.namespace ?? 'default') === namespace && (p.spec.clientGroupRefs ?? []).some((g) => groups.has(g)),
  );
  const perr = priorityError(spec.priority);
  return (
    <VStack gap={4}>
      <ObjectRefPicker
        kind="clientgroups"
        label="Client groups"
        description="Clients in these groups get this policy."
        namespace={namespace}
        sameNamespace
        multiple
        value={spec.clientGroupRefs ?? []}
        onChange={(v) => onChange({ ...spec, clientGroupRefs: v })}
        detail={groupDetail}
        isDisabled={isDisabled}
        error={showErrors && !spec.clientGroupRefs?.length ? 'Pick at least one group' : undefined}
      />
      <NumberField
        label="Priority"
        value={spec.priority === undefined ? '' : String(spec.priority)}
        min={PRIORITY_MIN}
        max={PRIORITY_MAX}
        isDisabled={isDisabled}
        error={perr ?? undefined}
        help="When several active policies match a client, the highest priority decides the settings; their lists are merged."
        onChange={(v) => onChange({ ...spec, priority: v === '' ? undefined : Number(v) })}
        width={260}
      />
      {others.length ? (
        <Text type="supporting" as="p" data-testid="priority-hint">
          Other policies of these groups:{' '}
          {others.map((p, i) => (
            <Text key={p.metadata.name} type="inherit">
              {i ? ', ' : ''}
              <Code color="inherit">{p.metadata.name}</Code> ({p.spec.priority ?? 0})
            </Text>
          ))}
          .
        </Text>
      ) : null}
    </VStack>
  );
}

// -- What ---------------------------------------------------------------------

export function WhatFields({ spec, onChange, namespace, isDisabled }: PolicyFieldsProps) {
  return (
    <VStack gap={4}>
      <ObjectRefPicker
        kind="blocklists"
        label="Block lists"
        description="Names on these lists are blocked."
        namespace={namespace}
        sameNamespace
        multiple
        filter={blockListFilter}
        detail={listDetail}
        value={spec.blockListRefs ?? []}
        onChange={(v) => onChange({ ...spec, blockListRefs: v })}
        isDisabled={isDisabled}
      />
      <ObjectRefPicker
        kind="blocklists"
        label="Allow lists"
        description="Names on these lists are never blocked, whatever a block list or service says."
        namespace={namespace}
        sameNamespace
        multiple
        filter={allowList}
        detail={listDetail}
        value={spec.allowListRefs ?? []}
        onChange={(v) => onChange({ ...spec, allowListRefs: v })}
        isDisabled={isDisabled}
      />
      <SwitchField
        label="Allow only"
        help="Block every name that is not on an allow list (a walled garden for young children)."
        value={spec.allowOnly === true}
        onChange={(on) => onChange({ ...spec, allowOnly: on || undefined })}
        isDisabled={isDisabled}
      />
    </VStack>
  );
}

export function ServicesFields({ spec, onChange, isDisabled, isLabelHidden }: PolicyFieldsProps & { isLabelHidden?: boolean }) {
  return (
    <ServiceGrid
      label="Blocked services"
      isLabelHidden={isLabelHidden}
      description="Block every domain of these services, like a block list. A blocked category also covers services later catalogue updates add to it. Allow lists still win; the query log names the service."
      value={selectionOf(spec)}
      onChange={(sel) => onChange(withSelection(spec, sel))}
      isDisabled={isDisabled}
    />
  );
}

// -- Content categories -------------------------------------------------------

/** Content categories (Block List Project lists, fetched by URL): a view over blockListRefs. */
export function ContentCategoryFields({ spec, onChange, isDisabled }: PolicyFieldsProps) {
  const refs = spec.blockListRefs ?? [];
  return (
    <VStack gap={2}>
      <Text type="supporting" display="block">
        Large community lists of sites by topic from the Block List Project, downloaded by kudurru from their URL and refreshed like any list. Turning one on creates the
        block list <Text type="code">blp-…</Text> in this namespace if it does not exist yet. {BLP_LICENCE_NOTE}
      </Text>
      {CONTENT_GROUPS.map((g) => (
        <Card key={g.id} padding={3}>
          <VStack as="fieldset" gap={3} style={FIELDSET_RESET}>
            <legend>
              <Text type="label">{g.name}</Text>
            </legend>
            <Grid columns={{ minWidth: 260 }} gap={3}>
              {CONTENT_CATEGORIES.filter((c) => c.group === g.id).map((c) => (
                <CheckboxField
                  key={c.id}
                  label={c.name}
                  help={`${c.description} ${c.size}.`}
                  value={refs.includes(contentListName(c.id))}
                  isDisabled={isDisabled}
                  onChange={(on) => onChange({ ...spec, blockListRefs: on ? [...refs.filter((r) => r !== contentListName(c.id)), contentListName(c.id)] : refs.filter((r) => r !== contentListName(c.id)) })}
                />
              ))}
            </Grid>
          </VStack>
        </Card>
      ))}
    </VStack>
  );
}

// -- Safe search --------------------------------------------------------------

export function SafeSearchFields({ spec, onChange, isDisabled }: PolicyFieldsProps) {
  const engines = useSafeSearchEngines();
  const on = spec.safeSearch === true;
  return (
    <VStack gap={4}>
      <SwitchField
        label="Enforce safe search"
        help="Answer the search engines' and YouTube's host names with the vendor's safe-search address."
        value={on}
        onChange={(v) => onChange({ ...spec, safeSearch: v || undefined, ...(v ? {} : { safeSearchMode: undefined }) })}
        isDisabled={isDisabled}
      />
      {on ? (
        <EnumSelect label="Level" value={spec.safeSearchMode ?? 'strict'} options={SAFE_SEARCH_MODES} onChange={(v) => onChange({ ...spec, safeSearchMode: v === 'strict' ? undefined : (v as 'moderate') })} isDisabled={isDisabled} />
      ) : null}
      {engines.data ? (
        <VStack gap={2}>
          <Text type="supporting">{on ? 'Rewritten engines' : 'Engines it would rewrite'}</Text>
          <EngineList engines={engines.data.items} mode={spec.safeSearchMode ?? 'strict'} />
        </VStack>
      ) : null}
    </VStack>
  );
}

// -- How ----------------------------------------------------------------------

const TTL_PRESETS: DurationPreset[] = [
  { value: '0s', label: 'No caching (0)' },
  { value: '10s', label: '10 seconds' },
  { value: '1m', label: '1 minute' },
  { value: '5m', label: '5 minutes' },
  { value: '10m', label: '10 minutes' },
  { value: '1h', label: '1 hour' },
];

export function howError(spec: PolicySpec): string | null {
  return spec.blockTTL ? (durationError(spec.blockTTL) ? 'Block TTL is a duration such as 30s, 10m or 1h.' : null) : null;
}

export function HowFields({ spec, onChange, isDisabled, showErrors }: PolicyFieldsProps) {
  const ttlErr = spec.blockTTL ? durationError(spec.blockTTL) : null;
  return (
    <VStack gap={4}>
      <EnumSelect
        label="Block response"
        description="What a blocked client gets back."
        value={spec.blockMode ?? ''}
        options={BLOCK_RESPONSES}
        emptyLabel="Default (blocking.defaultResponse)"
        onChange={(v) => onChange({ ...spec, blockMode: (v || undefined) as PolicySpec['blockMode'] })}
        isDisabled={isDisabled}
      />
      <DurationField
        label="Block TTL"
        description="How long clients may cache a block answer."
        value={spec.blockTTL ?? ''}
        presets={TTL_PRESETS}
        emptyLabel="Default (blocking.blockTTL)"
        onChange={(v) => onChange({ ...spec, blockTTL: v || undefined })}
        isDisabled={isDisabled}
        error={showErrors || spec.blockTTL ? (ttlErr ?? undefined) : undefined}
      />
      <UpstreamGroupSelect value={spec.upstreamGroup ?? ''} onChange={(v) => onChange({ ...spec, upstreamGroup: v || undefined })} isDisabled={isDisabled} help="Where these clients' allowed queries are forwarded." />
      <SwitchField
        label="Log these clients' queries"
        help="Off keeps the clients of this policy out of the query log and its statistics."
        value={spec.queryLog !== false}
        onChange={(on) => onChange({ ...spec, queryLog: on ? undefined : false })}
        isDisabled={isDisabled}
      />
    </VStack>
  );
}

// -- When ---------------------------------------------------------------------

export function whenError(spec: PolicySpec): string | null {
  return spec.schedule ? scheduleError(spec.schedule) : null;
}

export function WhenFields({ spec, onChange, isDisabled }: PolicyFieldsProps) {
  return <ScheduleField value={spec.schedule} onChange={(s) => onChange({ ...spec, schedule: s })} isDisabled={isDisabled} />;
}
