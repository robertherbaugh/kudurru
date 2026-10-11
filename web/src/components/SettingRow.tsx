// One settings key rendered from its metadata (§10 principle 10, C1): the
// human label with the path small underneath, a typed control chosen by the
// key's shape (lib/settingsMeta.ts, lib/shapes.ts), a "changed" badge and
// "Reset to default" when the value differs from the default, and "Edit as
// JSON" as the escape hatch for lists and maps. Host-only,
// environment-set and not-permitted keys show the value read-only with the
// reason. Rows are bound by data-configkey.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { useEffect, useId, useMemo, useState, type ReactNode } from 'react';
import type { SettingKey } from '../api/types';
import { displayValue, lockFor, parseValue } from '../lib/settings';
import { labelOf, metaFor, shapeOf, type UiMeta, type UiShape } from '../lib/settingsMeta';
import { editable, fromDraft, sameValue, shapeError, toDraft } from '../lib/shapes';
import { durationLabel } from './fields/DurationField';
import { ShapeField } from './fields/ShapeField';

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

export function SettingRow({ k, value, canWrite, onSave, meta, extra, summary }: SettingRowProps) {
  const id = useId();
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
    setBusy(true);
    try {
      await onSave(k, v);
      return true;
    } catch (e) {
      setError((e as Error).message);
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
      if (structured && composite) return <ShapeField label={label} isLabelHidden shape={shape} value={toDraft(shape, value)} onChange={() => {}} isDisabled />;
      return composite ? (
        <pre id={id} className="k-pre" aria-readonly="true" tabIndex={0}>
          {k.writeOnly ? '(hidden)' : shownText || '(empty)'}
        </pre>
      ) : (
        <input id={id} className="k-native-input" aria-label={label} readOnly aria-readonly="true" value={k.writeOnly ? '(hidden)' : shownText} />
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
        <textarea
          id={id}
          aria-label={`${label} (JSON)`}
          className="k-native-input k-mono"
          rows={Math.min(14, Math.max(3, text.split('\n').length))}
          value={text}
          onChange={(e) => setText(e.target.value)}
          spellCheck={false}
          aria-invalid={!!error}
        />
      );
    }
    if (k.writeOnly) {
      return <input id={id} className="k-native-input" aria-label={label} type="password" value={text} placeholder="unchanged" onChange={(e) => setText(e.target.value)} />;
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

  return (
    <div className="k-setting" data-configkey={k.path} data-locked={lock.locked ? 'true' : 'false'} data-changed={changed ? 'true' : 'false'}>
      <div className="k-stack-sm">
        <span className="k-setting-label">{label}</span>
        <span className="k-mono k-small k-muted k-setting-key">{k.path}</span>
        <span className="k-small">{k.help}</span>
        {extra}
        <span className="k-row">
          <Badge variant="neutral" label={k.scope} />
          {changed ? <Badge variant="blue" label="changed" /> : null}
          {k.hostOnly ? <Badge variant="orange" label="host only" /> : null}
          {k.envLocked ? <Badge variant="purple" label="environment" /> : null}
          {k.restartRequired ? <Badge variant="warning" label="restart" /> : null}
        </span>
      </div>
      <div className="k-stack k-setting-value">
        {control()}
        {lock.locked ? (
          <p className="k-small k-muted" data-testid={`lock-${k.path}`}>
            {lock.reason}
          </p>
        ) : (
          <>
            {error ? (
              <span className="k-error-text" role="alert">
                {error}
              </span>
            ) : null}
            <span className="k-row">
              {dirty && k.type !== 'bool' ? (
                <>
                  <Button label="Save" size="sm" variant="primary" isLoading={busy} onClick={() => void save()} />
                  <Button label="Cancel" size="sm" variant="ghost" onClick={cancel} />
                </>
              ) : (
                <span className="k-small k-muted">Default: {describeValue(k, shape, k.default)}</span>
              )}
              <span className="k-spacer" />
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
            </span>
          </>
        )}
      </div>
    </div>
  );
}
