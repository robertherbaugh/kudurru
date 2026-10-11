// One settings key rendered from its metadata (§10 principle 10, C1) as a
// two-column row, 60% explanation and 40% value (one column under 900px):
// on the left the human label, the path small underneath, the help and the
// badges; on the right a typed control chosen by the key's shape
// (lib/settingsMeta.ts, lib/shapes.ts) with the default or Save/Cancel,
// "Reset to default" and "Edit as JSON" (lists and maps) under it.
// Host-only, environment-set and not-permitted keys show the value
// read-only with the reason. Every row has help: the server's, or, when it
// sends none or only a few words, a sentence about the value's type from
// the key's shape. Rows are bound by data-configkey.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid, GridSpan } from '@astryxdesign/core/Grid';
import { useMediaQuery } from '@astryxdesign/core/hooks';
import { HStack } from '@astryxdesign/core/HStack';
import { StackItem } from '@astryxdesign/core/Stack';
import { Switch } from '@astryxdesign/core/Switch';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useEffect, useId, useMemo, useState, type ReactNode } from 'react';
import { ApiError } from '../api/client';
import type { SettingKey } from '../api/types';
import { displayValue, lockFor, parseValue } from '../lib/settings';
import { labelOf, metaFor, shapeOf, type UiMeta, type UiShape } from '../lib/settingsMeta';
import { editable, fromDraft, sameValue, shapeError, toDraft } from '../lib/shapes';
import { durationLabel } from './fields/DurationField';
import { FORMAT_EXAMPLES } from './fields/formats';
import { rangeText } from './fields/NumberField';
import { ShapeField } from './fields/ShapeField';
import { TextAreaField, TextField } from './fields/TextField';

export interface SettingRowProps {
  k: SettingKey;
  value: unknown;
  canWrite: boolean;
  /** Saves a value (null resets the key to its default); rejects with a user-facing error. */
  onSave: (k: SettingKey, value: unknown) => Promise<void>;
  /** Metadata (default: metaFor(k)). */
  meta?: UiMeta;
  /** Shown under the help text (e.g. a link to a dedicated editor). */
  extra?: ReactNode;
  /** Replaces the structured editor (upstreams.groups shows a summary and a link). */
  summary?: ReactNode;
}

/** The parts of a key a 400 named (error.fields), relative to the key: "corp › strategy". */
export function failedParts(k: SettingKey, err: unknown): string[] {
  const api = err instanceof ApiError ? err : err instanceof Error && err.cause instanceof ApiError ? err.cause : undefined;
  return (api?.fields ?? [])
    .filter((f) => f !== k.path)
    .map((f) => (f.startsWith(`${k.path}.`) ? f.slice(k.path.length + 1).split('.').join(' › ') : f));
}

/** A value in words for the "Default:" line: enum labels, durations, lists. */
export function describeValue(k: SettingKey, shape: UiShape, v: unknown): string {
  if (v === undefined || v === null || v === '') return '(empty)';
  if (typeof v === 'string' && shape.enum?.length) return shape.enum.find((o) => o.value === v)?.label ?? v;
  if (k.type === 'duration' && typeof v === 'string') return `${durationLabel(v)} (${v})`;
  if (k.type === 'bool') return v === true ? 'on' : 'off';
  if (Array.isArray(v)) return v.length ? v.map((x) => (typeof x === 'string' ? x : JSON.stringify(x))).join(', ') : '(none)';
  if (typeof v === 'object') return Object.keys(v).length ? JSON.stringify(v) : '(none)';
  return displayValue(k, v);
}

/** What kind of object a reference names, for the type help. */
const REF_NOUNS: Record<string, string> = {
  clientGroup: 'client group',
  blockList: 'block or allow list',
  policy: 'policy',
  zone: 'zone',
  upstreamGroup: 'upstream group (configured on the Upstreams page)',
};

/** A sentence about a value's type from its shape, for keys whose server help is missing or thin. */
export function typeHelp(shape: UiShape): string {
  const range = rangeText(shape.min ?? undefined, shape.max ?? undefined);
  switch (shape.type) {
    case 'bool':
      return 'On or off.';
    case 'int':
      return range ? `A whole number, ${range}.` : 'A whole number.';
    case 'duration':
      return 'A duration such as 30s, 10m or 1h.';
    case 'bytes':
      return 'A size such as 500MB or 10GiB; 0 means no limit.';
    case 'string[]':
      return shape.items?.enum?.length
        ? `A list; each entry is one of ${shape.items.enum.map((o) => o.label).join(', ')}.`
        : shape.items?.format
          ? `A list, one entry per row, each like ${FORMAT_EXAMPLES[shape.items.format]}.`
          : 'A list, one entry per row.';
    case 'list':
      return 'A list of entries, edited row by row.';
    case 'map':
      return `Entries by ${(shape.keyLabel ?? 'key').toLowerCase()}, edited row by row.`;
    default:
      if (shape.enum?.length) return `One of ${shape.enum.length} choices; the chosen one is explained under the control.`;
      if (shape.ref) return `The name of a ${REF_NOUNS[shape.ref] ?? shape.ref}.`;
      if (shape.format) return `For example ${FORMAT_EXAMPLES[shape.format]}.`;
      return 'Free text.';
  }
}

/** The row's help: the server's, completed by the type help when it says little. */
export function rowHelp(k: SettingKey, shape: UiShape): string {
  const own = k.help.trim();
  if (own.split(/\s+/).filter(Boolean).length >= 4) return own;
  const t = typeHelp(shape);
  return own ? `${own.replace(/\.?$/, '.')} ${t}` : t;
}

export function SettingRow({ k, value, canWrite, onSave, meta, extra, summary }: SettingRowProps) {
  const id = useId();
  const narrow = useMediaQuery('(max-width: 900px)');
  const m = useMemo(() => meta ?? metaFor(k), [meta, k]);
  const shape = useMemo(() => shapeOf(k, m), [k, m]);
  const label = labelOf(k, m);
  const lock = lockFor(k, canWrite);
  const structured = !k.writeOnly && editable(shape);
  const composite = k.type === 'list' || k.type === 'map' || k.type === 'string[]';

  // The structured draft and the JSON/text fallback.
  const baseline = useMemo(() => (structured ? fromDraft(shape, toDraft(shape, value)) : value), [structured, shape, value]);
  const shownText = k.writeOnly ? '' : displayValue(k, value);
  const [draft, setDraft] = useState<unknown>(() => toDraft(shape, value));
  const [text, setText] = useState(shownText);
  const [asJSON, setAsJSON] = useState(!structured && composite);
  const [error, setError] = useState<string | undefined>();
  const [parts, setParts] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const valueKey = JSON.stringify(value ?? null);
  useEffect(() => {
    setDraft(toDraft(shape, value));
    setText(k.writeOnly ? '' : displayValue(k, value));
    setError(undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- reset only when the saved value changes
  }, [valueKey, shape]);

  const jsonMode = asJSON || (!structured && composite);
  const dirty = jsonMode || !structured ? text !== shownText : !sameValue(k.type, fromDraft(shape, draft), baseline);
  const changed = !k.writeOnly && !sameValue(k.type, value, k.default);

  const run = async (v: unknown): Promise<boolean> => {
    setError(undefined);
    setParts([]);
    setBusy(true);
    try {
      await onSave(k, v);
      return true;
    } catch (e) {
      // The key is on screen already; "settings: " says nothing here.
      setError((e as Error).message.replace(/^settings: /, ''));
      setParts(failedParts(k, e));
      return false;
    } finally {
      setBusy(false);
    }
  };

  const save = async () => {
    if (jsonMode || !structured) {
      let parsed: unknown;
      try {
        parsed = parseValue(k, text);
      } catch (e) {
        setError((e as Error).message);
        return;
      }
      await run(parsed);
      return;
    }
    const e = shapeError(shape, draft);
    if (e) {
      setError(e);
      return;
    }
    await run(fromDraft(shape, draft));
  };

  const cancel = () => {
    setDraft(toDraft(shape, value));
    setText(shownText);
    setError(undefined);
  };

  const control = () => {
    if (lock.locked) {
      if (k.type === 'bool' && !k.writeOnly) return <Switch label={label} isLabelHidden value={value === true} isDisabled onChange={() => {}} />;
      if (structured && composite) return <ShapeField label={label} isLabelHidden shape={shape} value={toDraft(shape, value)} onChange={() => {}} isDisabled />;
      return composite ? (
        <pre id={id} className="k-pre" aria-readonly="true" tabIndex={0}>
          {k.writeOnly ? '(hidden)' : shownText || '(empty)'}
        </pre>
      ) : (
        <TextField label={label} isLabelHidden isReadOnly value={k.writeOnly ? '(hidden)' : shownText} />
      );
    }
    if (k.type === 'bool') {
      return (
        <Switch
          label={label}
          isLabelHidden
          value={draft === true}
          isLoading={busy}
          onChange={(on) => {
            setDraft(on);
            void run(on).then((ok) => {
              if (!ok) setDraft(!on);
            });
          }}
        />
      );
    }
    if (jsonMode) {
      return (
        <TextAreaField
          label={`${label} (JSON)`}
          isLabelHidden
          mono
          rows={Math.min(14, Math.max(3, text.split('\n').length))}
          value={text}
          onChange={setText}
          hasSpellCheck={false}
        />
      );
    }
    if (k.writeOnly) {
      return <TextField label={label} isLabelHidden type="password" value={text} placeholder="unchanged" onChange={setText} autoComplete="new-password" />;
    }
    if (summary) return summary;
    return (
      <ShapeField
        label={label}
        isLabelHidden
        shape={shape}
        value={draft}
        onChange={setDraft}
        placeholder={k.type === 'string' ? displayValue(k, k.default) : undefined}
      />
    );
  };

  const help = rowHelp(k, shape);
  return (
    <Grid
      columns={narrow ? 1 : 5}
      columnGap={6}
      rowGap={2}
      align="start"
      className="k-setting"
      data-configkey={k.path}
      data-locked={lock.locked ? 'true' : 'false'}
      data-changed={changed ? 'true' : 'false'}
    >
      <GridSpan columns={narrow ? 'full' : 3}>
        <VStack gap={1}>
          <Text weight="semibold" className="k-setting-label">
            {label}
          </Text>
          <Text type="supporting" className="k-mono k-setting-key">
            {k.path}
          </Text>
          <Text type="supporting" display="block" data-testid={`help-${k.path}`}>
            {help}
          </Text>
          {extra}
          <HStack gap={1} wrap="wrap">
            <Badge variant="neutral" label={k.scope} />
            {changed ? <Badge variant="blue" label="changed" /> : null}
            {k.hostOnly ? <Badge variant="orange" label="host only" /> : null}
            {k.envLocked ? <Badge variant="purple" label="environment" /> : null}
            {k.restartRequired ? <Badge variant="warning" label="restart" /> : null}
          </HStack>
        </VStack>
      </GridSpan>
      <GridSpan columns={narrow ? 'full' : 2}>
        <VStack gap={2} className="k-setting-value">
          {control()}
          {lock.locked ? (
            <Text type="supporting" display="block" data-testid={`lock-${k.path}`}>
              {lock.reason}
            </Text>
          ) : (
            <>
              {error ? <FieldStatus type="error" variant="detached" role="alert" data-testid="setting-error" message={parts.length ? `${error} (at ${parts.join(', ')})` : error} /> : null}
              <HStack gap={2} wrap="wrap" vAlign="center">
                {dirty && k.type !== 'bool' ? (
                  <>
                    <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
                    <Button label="Cancel" size="sm" variant="ghost" onClick={cancel} />
                  </>
                ) : (
                  <Text type="supporting">Default: {describeValue(k, shape, k.default)}</Text>
                )}
                <StackItem size="fill" />
                {structured && composite && !summary ? (
                  <Button
                    label={asJSON ? 'Edit as form' : 'Edit as JSON'}
                    size="sm"
                    variant="ghost"
                    onClick={() => {
                      cancel();
                      setAsJSON(!asJSON);
                    }}
                  />
                ) : summary ? (
                  <Button label={asJSON ? 'Show summary' : 'Edit as JSON'} size="sm" variant="ghost" onClick={() => setAsJSON(!asJSON)} />
                ) : null}
                {changed && !dirty ? <Button label="Reset to default" size="sm" variant="ghost" isLoading={busy} onClick={() => void run(null)} /> : null}
              </HStack>
            </>
          )}
        </VStack>
      </GridSpan>
    </Grid>
  );
}
