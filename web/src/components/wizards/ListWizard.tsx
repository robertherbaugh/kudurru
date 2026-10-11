// New list, in a WizardDialog over the Lists page (?new=1): Type → Sources
// → Options → Review. Sources are URL rows (format detected from the URL),
// pasted entries, or well-known public lists the user opts into (names and
// URLs only; kudurru embeds none of them). The finished list is created
// with POST /objects/blocklists and, when policies are chosen, referenced
// from them so it takes effect; then the dialog closes and the list's page
// opens.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxList, CheckboxListItem } from '@astryxdesign/core/CheckboxList';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Collapsible } from '@astryxdesign/core/Collapsible';
import { FormLayout } from '@astryxdesign/core/FormLayout';
import { Heading } from '@astryxdesign/core/Heading';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useMemo, useState } from 'react';
import { qk, useApi, useCan } from '../../api/context';
import type { BlockList, BlockListSpec, BlockResponse, ListFormat, ListSource, Policy } from '../../api/types';
import { DurationField } from '../fields/DurationField';
import { ChoiceCards } from '../fields/ChoiceCards';
import { FieldShell } from '../fields/FieldHelp';
import { ObjectRefPicker } from '../fields/ObjectRefPicker';
import { SelectField } from '../fields/SelectField';
import { SwitchField, TextAreaField, TextField } from '../fields/TextField';
import { errorText, useNotify } from '../Feedback';
import { slugName, suggestName } from '../lists/SourceDialog';
import { Wizard, WizardDialog } from '../Wizard';
import { applyable } from '../YamlDialog';
import { formatExact } from '../../lib/format';
import { CURATED_LISTS, entriesProblem, FORMAT_OPTIONS, formatLabel, guessFormat, isHTTPURL, listPath, parseEntries, RESPONSE_OPTIONS } from '../../lib/lists';
import { COMMENT, ENABLED, objectOwnership } from '../../lib/managedBy';
import { durationError, nameError } from '../../lib/objectSchemas';
import { useNavigate } from '../../lib/router';
import { isIPv4, isIPv6 } from '../../lib/rr';
import { toYAML } from '../../lib/yaml';
import { REFRESH_PRESETS } from '../../pages/ListDetailPage';

/** A row of the review's sources table. */
interface SourceRow extends Record<string, unknown> {
  name: string;
  from: string;
  format: string;
}

interface UrlRow {
  id: number;
  url: string;
  format: ListFormat;
  formatTouched: boolean;
}

export interface WizardState {
  mode: 'block' | 'allow';
  curated: string[];
  urls: UrlRow[];
  paste: string;
  pasteFormat: ListFormat;
  name: string;
  /** False while the name follows the suggestion. */
  nameTouched: boolean;
  namespace: string;
  comment: string;
  refreshInterval: string;
  response: string;
  v4: string;
  v6: string;
  enabled: boolean;
  policies: string[] | null;
}

const EMPTY: WizardState = {
  mode: 'block',
  curated: [],
  urls: [],
  paste: '',
  pasteFormat: 'auto',
  name: '',
  nameTouched: false,
  namespace: 'default',
  comment: '',
  refreshInterval: '',
  response: '',
  v4: '',
  v6: '',
  enabled: true,
  policies: null,
};

/** The sources the wizard state describes, named uniquely. */
export function wizardSources(s: WizardState): ListSource[] {
  const out: ListSource[] = [];
  const taken = () => out.map((x) => x.name ?? '');
  for (const id of s.curated) {
    const c = CURATED_LISTS.find((x) => x.id === id);
    if (c) out.push({ name: c.source, url: c.url, format: c.format });
  }
  for (const r of s.urls) {
    if (r.url.trim()) out.push({ name: suggestName(r.url.trim(), taken()), url: r.url.trim(), format: r.format });
  }
  const entries = parseEntries(s.paste);
  if (entries.length) out.push({ name: slugName('custom', taken()), inline: entries, format: s.pasteFormat });
  return out;
}

/** A name suggestion: the only curated list's source name, else "<mode>-list". */
function suggestListName(s: WizardState): string {
  const src = wizardSources(s);
  if (src.length === 1 && src[0]?.name && src[0].url) return src[0].name;
  return s.mode === 'allow' ? 'my-allow-list' : 'my-block-list';
}

export function wizardObject(s: WizardState): BlockList {
  const spec: BlockListSpec = { mode: s.mode, sources: wizardSources(s) };
  if (s.refreshInterval.trim() && spec.sources.some((x) => x.url)) spec.refreshInterval = s.refreshInterval.trim();
  if (s.mode === 'block' && s.response) spec.response = s.response as BlockResponse;
  if (spec.response === 'customIP') spec.customIP = { ...(s.v4.trim() ? { v4: s.v4.trim() } : {}), ...(s.v6.trim() ? { v6: s.v6.trim() } : {}) };
  const annotations: Record<string, string> = {};
  if (s.comment.trim()) annotations[COMMENT] = s.comment.trim();
  if (!s.enabled) annotations[ENABLED] = 'false';
  return {
    apiVersion: 'kudurru.io/v1alpha1',
    kind: 'BlockList',
    metadata: { name: s.name.trim(), namespace: s.namespace, ...(Object.keys(annotations).length ? { annotations } : {}) },
    spec,
  };
}

/** Policy p with the list added to its block or allow refs (bare name in the same namespace). */
export function withListRef(p: Policy, list: BlockList): Policy {
  const ns = list.metadata.namespace ?? 'default';
  const ref = (p.metadata.namespace ?? 'default') === ns ? list.metadata.name : `${ns}/${list.metadata.name}`;
  const field = list.spec.mode === 'allow' ? 'allowListRefs' : 'blockListRefs';
  const refs = p.spec[field] ?? [];
  if (refs.includes(ref)) return p;
  return { ...p, spec: { ...p.spec, [field]: [...refs, ref] } };
}

export interface ListWizardProps {
  /** Closes the dialog without asking (after the list was created). */
  onClose: () => void;
}

export function ListWizardDialog({ isOpen, onClose }: ListWizardProps & { isOpen: boolean }) {
  return (
    <WizardDialog isOpen={isOpen} onClose={onClose} title="New list" subtitle="A block or allow list from public lists, URLs or your own entries." what="list">
      <ListWizard onClose={onClose} />
    </WizardDialog>
  );
}

function ListWizard({ onClose }: ListWizardProps) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [s, setS] = useState<WizardState>(EMPTY);
  const [nextId, setNextId] = useState(1);
  const [finishing, setFinishing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const set = (patch: Partial<WizardState>) => setS((prev) => ({ ...prev, ...patch }));
  const urlsId = useId();
  const lists = useQuery({ queryKey: qk.objects('blocklists'), queryFn: () => api.objects.list<BlockList>('blocklists') });
  const policies = useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies') });

  // Default: the namespace's default policy when the API may change it.
  const defaultPolicies = useMemo(() => {
    const p = policies.data?.items.find((x) => x.metadata.name === 'default' && (x.metadata.namespace ?? 'default') === s.namespace);
    return p && objectOwnership(p).editable ? [`${s.namespace}/default`] : [];
  }, [policies.data, s.namespace]);
  const chosenPolicies = s.policies ?? defaultPolicies;

  const sources = wizardSources(s);
  const name = s.nameTouched ? s.name : suggestListName(s);
  const pasteEntries = parseEntries(s.paste);
  const pasteProblem = pasteEntries.length ? entriesProblem(pasteEntries, s.pasteFormat) : null;
  const hasUrl = sources.some((x) => x.url);

  const setUrl = (id: number, patch: Partial<UrlRow>) => set({ urls: s.urls.map((r) => (r.id === id ? { ...r, ...patch } : r)) });

  const validateSources = () => {
    if (!sources.length) return 'Add at least one source: pick a well-known list, add a URL or paste entries.';
    const bad = s.urls.find((r) => r.url.trim() && !isHTTPURL(r.url));
    if (bad) return `"${bad.url}" is not an http(s) URL.`;
    const urls = sources.flatMap((x) => (x.url ? [x.url] : []));
    const dup = urls.find((u, i) => urls.indexOf(u) !== i);
    if (dup) return `${dup} is listed twice.`;
    if (pasteProblem) return `Pasted entries: ${formatExact(pasteProblem.count)} invalid, first "${pasteProblem.first}": ${pasteProblem.error}`;
    return null;
  };

  const optionErrors = {
    name: nameError(name) ?? (lists.data?.items.some((l) => l.metadata.name === name && (l.metadata.namespace ?? 'default') === s.namespace) ? `A list named ${name} exists already` : null),
    refreshInterval: durationError(s.refreshInterval.trim()),
    v4: s.v4 && !isIPv4(s.v4.trim()) ? 'Not an IPv4 address' : s.response === 'customIP' && !s.v4.trim() && !s.v6.trim() ? 'Give an IPv4 or IPv6 address' : null,
    v6: s.v6 && !isIPv6(s.v6.trim()) ? 'Not an IPv6 address' : null,
  };
  const [showOptionErrors, setShowOptionErrors] = useState(false);
  const optErr = (k: keyof typeof optionErrors) => (showOptionErrors ? (optionErrors[k] ?? undefined) : undefined);

  const finish = async () => {
    const obj = wizardObject({ ...s, name });
    setFinishing(true);
    setError(null);
    try {
      const created = await api.objects.create<BlockList>('blocklists', obj);
      const failed: string[] = [];
      for (const ref of chosenPolicies) {
        const [pns, pname] = ref.includes('/') ? (ref.split('/') as [string, string]) : ['default', ref];
        try {
          const p = await api.objects.get<Policy>('policies', pname, pns);
          const next = withListRef(p, created);
          if (next !== p) await api.objects.put('policies', next);
        } catch (e) {
          failed.push(`${ref}: ${errorText(e)}`);
        }
      }
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
      void qc.invalidateQueries({ queryKey: qk.objects('policies') });
      notify.success(`Created ${created.metadata.name}`);
      for (const f of failed) notify.error(new Error(f), 'Could not add the list to a policy');
      onClose();
      navigate(listPath(created));
    } catch (e) {
      setError(errorText(e));
    } finally {
      setFinishing(false);
    }
  };

  const typeStep = (
    <ChoiceCards
      label="List type"
      isLabelHidden
      value={s.mode}
      onChange={(v) => set({ mode: v as WizardState['mode'], ...(v === 'allow' ? { curated: [], response: '' } : {}) })}
      choices={[
        { value: 'block', label: 'Block list', description: 'Names on the list are blocked for the client groups of the policies that use it: ads, trackers, malware, or anything you choose.' },
        { value: 'allow', label: 'Allow list', description: 'Names on the list are never blocked, whatever the block lists of the same policy say. Use it for sites a block list breaks.' },
      ]}
    />
  );

  const sourcesStep = (
    <VStack gap={5}>
      {s.mode === 'block' ? (
        <FieldShell help="Published by their maintainers under their own licences; kudurru only fetches the URL you opt into." describes="group">
          {() => (
            <CheckboxList label="Well-known lists" value={s.curated} onChange={(v) => set({ curated: v })} hasDividers>
              {CURATED_LISTS.map((c) => (
                <CheckboxListItem
                  key={c.id}
                  value={c.id}
                  label={`${c.publisher} ${c.name}`}
                  endContent={<Badge variant="neutral" label={c.licence} />}
                  description={
                    <VStack gap={0.5}>
                      <Text type="supporting">
                        {c.size}, {formatLabel(c.format).toLowerCase()}. {c.description}
                      </Text>
                      <Text type="code" size="sm" color="secondary" wordBreak="break-all">
                        {c.url}
                      </Text>
                    </VStack>
                  }
                />
              ))}
            </CheckboxList>
          )}
        </FieldShell>
      ) : null}

      <VStack gap={2} role="group" aria-labelledby={urlsId}>
        <VStack gap={0.5}>
          <Heading level={4} id={urlsId}>
            List URLs
          </Heading>
          <Text type="supporting">Any hosts, domains, adblock or regex list served over http(s). The format is detected from the URL; change it if it is wrong.</Text>
        </VStack>
        {s.urls.map((r, i) => (
          <HStack key={r.id} gap={2} vAlign="start">
            <StackItem size="fill">
              <TextField
                label={`URL ${i + 1}`}
                mono
                placeholder="https://example.com/blocklist.txt"
                value={r.url}
                onChange={(v) => setUrl(r.id, { url: v, ...(r.formatTouched ? {} : { format: guessFormat(v) }) })}
                error={r.url.trim() && !isHTTPURL(r.url) ? 'An http(s) URL' : undefined}
              />
            </StackItem>
            <SelectField label={`Format of URL ${i + 1}`} value={r.format} options={FORMAT_OPTIONS} onChange={(v) => setUrl(r.id, { format: v as ListFormat, formatTouched: true })} />
            <StackItem crossAlignSelf="end">
              <Button label="Remove" variant="ghost" onClick={() => set({ urls: s.urls.filter((x) => x.id !== r.id) })} />
            </StackItem>
          </HStack>
        ))}
        <HStack gap={2}>
          <Button
            label={s.urls.length ? 'Add another URL' : 'Add a URL'}
            variant="secondary"
            onClick={() => {
              set({ urls: [...s.urls, { id: nextId, url: '', format: 'auto', formatTouched: false }] });
              setNextId((n) => n + 1);
            }}
          />
        </HStack>
      </VStack>

      <VStack gap={2}>
        <TextAreaField
          label="Paste entries"
          isOptional
          mono
          placeholder={s.mode === 'allow' ? 's.youtube.com\n*.cdn.example.com' : 'ads.example.com\n||tracker.example^'}
          rows={4}
          hasSpellCheck={false}
          value={s.paste}
          onChange={(v) => set({ paste: v })}
          help="Your own entries, one per line; they become an inline source you can browse and edit on the list page."
          error={pasteProblem ? `"${pasteProblem.first}": ${pasteProblem.error}` : undefined}
        />
        {pasteEntries.length ? <SelectField label="Format of the pasted entries" value={s.pasteFormat} options={FORMAT_OPTIONS} onChange={(v) => set({ pasteFormat: v as ListFormat })} /> : null}
      </VStack>
      <Text type="supporting" role="status">
        {sources.length ? `${sources.length} ${sources.length === 1 ? 'source' : 'sources'} chosen.` : 'No source chosen yet.'}
      </Text>
    </VStack>
  );

  const optionsStep = (
    <VStack gap={5}>
      <FormLayout direction="horizontal">
        <TextField
          label="Name"
          mono
          value={name}
          onChange={(v) => set({ name: v, nameTouched: true })}
          help="Lower-case letters, digits and dashes; policies refer to the list by it."
          error={optErr('name')}
        />
        <TextField label="Comment" placeholder="What the list is for" value={s.comment} onChange={(v) => set({ comment: v })} isOptional />
      </FormLayout>
      {hasUrl || s.mode === 'block' ? (
        <FormLayout direction="horizontal">
          {hasUrl ? (
            <DurationField label="Refresh interval" description="How often the URLs are fetched again." value={s.refreshInterval} presets={REFRESH_PRESETS} emptyLabel="Default (every day)" error={optErr('refreshInterval')} onChange={(v) => set({ refreshInterval: v })} />
          ) : null}
          {s.mode === 'block' ? (
            <SelectField label="Block response" value={s.response} options={RESPONSE_OPTIONS} emptyLabel="Default (the policy's, else the server's)" onChange={(v) => set({ response: v })} />
          ) : null}
        </FormLayout>
      ) : null}
      {s.mode === 'block' && s.response === 'customIP' ? (
        <FormLayout direction="horizontal">
          <TextField label="Custom IPv4" mono placeholder="192.0.2.1" value={s.v4} onChange={(v) => set({ v4: v })} error={optErr('v4')} />
          <TextField label="Custom IPv6" mono placeholder="2001:db8::1" value={s.v6} onChange={(v) => set({ v6: v })} isOptional error={optErr('v6')} />
        </FormLayout>
      ) : null}
      <ObjectRefPicker
        kind="policies"
        label="Use in policies"
        description={`The list takes effect for the client groups of these policies (as ${s.mode === 'allow' ? 'an allow' : 'a block'} list). Policies managed elsewhere are not listed.`}
        multiple
        value={chosenPolicies}
        filter={(o) => objectOwnership(o).editable}
        isDisabled={!can('policies', 'write')}
        onChange={(v) => set({ policies: v })}
      />
      <SwitchField label="Start enabled" help="A disabled list is stored but not fetched or applied." value={s.enabled} onChange={(v) => set({ enabled: v })} />
    </VStack>
  );

  const obj = wizardObject({ ...s, name });
  const reviewStep = (
    <VStack gap={5}>
      <MetadataList columns="multi">
        <MetadataListItem label="Type">{s.mode === 'allow' ? 'Allow list' : 'Block list'}</MetadataListItem>
        <MetadataListItem label="Name">
          <Text type="code">
            {s.namespace}/{name}
          </Text>
        </MetadataListItem>
        <MetadataListItem label="Starts">{s.enabled ? 'enabled' : 'disabled'}</MetadataListItem>
        <MetadataListItem label="Used by">{chosenPolicies.length ? chosenPolicies.join(', ') : 'no policy yet'}</MetadataListItem>
        {hasUrl ? <MetadataListItem label="Refresh">{s.refreshInterval || 'every day (default)'}</MetadataListItem> : null}
        {s.mode === 'block' ? <MetadataListItem label="Block response">{RESPONSE_OPTIONS.find((o) => o.value === s.response)?.label ?? 'default'}</MetadataListItem> : null}
      </MetadataList>
      <Table<SourceRow>
        aria-label="Sources to create"
        density="compact"
        idKey="name"
        data={obj.spec.sources.map((x) => ({ name: x.name ?? '', from: x.url ?? `${formatExact(x.inline?.length ?? 0)} inline entries`, format: formatLabel(x.format) }))}
        columns={[
          { key: 'name', header: 'Source', renderCell: (r) => <Text type="code">{r.name}</Text> },
          { key: 'from', header: 'From', width: proportional(3), renderCell: (r) => <Text type="code" size="sm" wordBreak="break-all">{r.from}</Text> },
          { key: 'format', header: 'Format' },
        ]}
      />
      {!chosenPolicies.length ? <Banner status="warning" title="No policy uses this list" description="It is created but has no effect until a policy references it." /> : null}
      <Collapsible trigger="YAML" defaultIsOpen={false}>
        <CodeBlock code={toYAML(applyable(obj))} language="yaml" />
      </Collapsible>
    </VStack>
  );

  return (
    <Wizard
      label="New list"
      finishLabel="Create list"
      isFinishing={finishing}
      onFinish={finish}
      isDirty={s !== EMPTY}
      error={error ? <Banner status="error" title="Could not create the list" description={error} /> : null}
      steps={[
          { id: 'type', label: 'Type', description: 'What the entries of the list do.', content: typeStep },
          { id: 'sources', label: 'Sources', description: 'Where the entries come from. You can add, turn off and remove sources later.', content: sourcesStep, validate: validateSources },
          {
            id: 'options',
            label: 'Options',
            content: optionsStep,
            validate: () => {
              setShowOptionErrors(true);
              return Object.values(optionErrors).find(Boolean) ?? null;
            },
          },
          { id: 'review', label: 'Review', description: 'Nothing is saved until you create the list.', content: reviewStep },
        ]}
    />
  );
}
