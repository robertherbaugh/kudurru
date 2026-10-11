// One settings key rendered from its metadata (§10 principle 10): a typed
// editor bound by data-configkey, or the value read-only with the reason
// when the key is host-only, set by the environment, or not writable for
// this principal.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { useEffect, useId, useState, type ReactNode } from 'react';
import type { SettingKey } from '../api/types';
import { displayValue, lockFor, parseValue } from '../lib/settings';

export interface SettingRowProps {
  k: SettingKey;
  value: unknown;
  canWrite: boolean;
  /** Saves a parsed value; rejects with a user-facing error. */
  onSave: (k: SettingKey, value: unknown) => Promise<void>;
  /** Shown under the help text (e.g. a link to a dedicated editor). */
  extra?: ReactNode;
}

export function SettingRow({ k, value, canWrite, onSave, extra }: SettingRowProps) {
  const id = useId();
  const lock = lockFor(k, canWrite);
  const shown = k.writeOnly ? '' : displayValue(k, value);
  const [text, setText] = useState(shown);
  const [error, setError] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  useEffect(() => setText(shown), [shown]);
  const dirty = text !== shown;
  const multiline = k.type === 'list' || k.type === 'map' || k.type === 'string[]';

  const save = async (raw: string) => {
    let parsed: unknown;
    try {
      parsed = parseValue(k, raw);
    } catch (e) {
      setError((e as Error).message);
      return;
    }
    setError(undefined);
    setBusy(true);
    try {
      await onSave(k, parsed);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="k-setting" data-configkey={k.path} data-locked={lock.locked ? 'true' : 'false'}>
      <div className="k-stack">
        <label htmlFor={id} className="k-mono k-setting-key">
          {k.path}
        </label>
        <span className="k-small">{k.help}</span>
        {extra}
        <span className="k-row">
          <Badge variant="neutral" label={k.scope} />
          <Badge variant="neutral" label={k.type} />
          {k.hostOnly ? <Badge variant="orange" label="host only" /> : null}
          {k.envLocked ? <Badge variant="purple" label="environment" /> : null}
          {k.restartRequired ? <Badge variant="warning" label="restart" /> : null}
        </span>
      </div>
      <div className="k-stack k-setting-value">
        {lock.locked ? (
          <>
            {multiline ? (
              <pre id={id} className="k-pre" aria-readonly="true" tabIndex={0}>
                {k.writeOnly ? '(hidden)' : shown || '(empty)'}
              </pre>
            ) : (
              <input id={id} className="k-native-input" readOnly aria-readonly="true" value={k.writeOnly ? '(hidden)' : shown} />
            )}
            <p className="k-small k-muted" data-testid={`lock-${k.path}`}>
              {lock.reason}
            </p>
          </>
        ) : k.type === 'bool' ? (
          <Switch
            label={k.path}
            isLabelHidden
            value={text === 'true'}
            isLoading={busy}
            onChange={(on) => {
              setText(String(on));
              void save(String(on));
            }}
          />
        ) : (
          <form
            className="k-stack"
            onSubmit={(e) => {
              e.preventDefault();
              void save(text);
            }}
          >
            {multiline ? (
              <textarea id={id} className="k-native-input k-mono" rows={Math.min(10, Math.max(3, text.split('\n').length))} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} aria-invalid={!!error} />
            ) : (
              <input
                id={id}
                className="k-native-input"
                type={k.writeOnly ? 'password' : 'text'}
                value={text}
                placeholder={k.writeOnly ? 'unchanged' : displayValue(k, k.default)}
                onChange={(e) => setText(e.target.value)}
                aria-invalid={!!error}
              />
            )}
            {error ? (
              <span className="k-error-text" role="alert">
                {error}
              </span>
            ) : null}
            {dirty ? (
              <span className="k-row">
                <Button type="submit" label="Save" size="sm" variant="primary" isLoading={busy} />
                <Button label="Reset" size="sm" variant="ghost" onClick={() => setText(shown)} />
              </span>
            ) : (
              <span className="k-small k-muted">Default: {displayValue(k, k.default) || '(empty)'}</span>
            )}
          </form>
        )}
      </div>
    </div>
  );
}
