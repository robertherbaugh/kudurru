// A select of the configured upstream groups (upstreams.groups of
// GET /settings) for the policy and forward-zone forms. Without
// settings.read it falls back to a text input.
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId } from 'react';
import { DEFAULT_GROUP, useUpstreamGroups } from '../lib/upstreams';
import { Link } from '../lib/router';

export interface UpstreamGroupSelectProps {
  label?: string;
  value: string;
  onChange: (v: string) => void;
  /** Offer "not set" (the policy or zone then uses the default group). */
  allowEmpty?: boolean;
  isDisabled?: boolean;
  help?: string;
  error?: string;
}

export function UpstreamGroupSelect({ label = 'Upstream group', value, onChange, allowEmpty = true, isDisabled, help, error }: UpstreamGroupSelectProps) {
  const id = useId();
  const groups = useUpstreamGroups();
  if (!groups.available) {
    return (
      <TextInput
        label={label}
        description={help}
        placeholder={DEFAULT_GROUP}
        value={value}
        onChange={onChange}
        isDisabled={isDisabled}
        status={error ? { type: 'error', message: error } : undefined}
      />
    );
  }
  const names = [...groups.names];
  if (value && !names.includes(value)) names.push(value);
  return (
    <label className="k-label" htmlFor={id}>
      {label}
      <select id={id} className="k-native-select" value={value} disabled={isDisabled} onChange={(e) => onChange(e.target.value)}>
        {allowEmpty ? <option value="">Not set ({DEFAULT_GROUP})</option> : null}
        {names.map((n) => (
          <option key={n} value={n}>
            {n}
            {groups.names.includes(n) ? '' : ' (not configured)'}
          </option>
        ))}
      </select>
      <span className="k-muted k-small">
        {help ? `${help} ` : ''}
        Groups are configured on the <Link href="/upstreams">Upstreams</Link> page.
      </span>
      {error ? <span className="k-error-text">{error}</span> : null}
    </label>
  );
}
