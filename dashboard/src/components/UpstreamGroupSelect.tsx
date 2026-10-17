// A Selector of the configured upstream groups (upstreams.groups of
// GET /settings) for the policy and forward-zone forms. Without
// settings.read it falls back to a text input.
import { DEFAULT_GROUP, useUpstreamGroups } from '../lib/upstreams';
import { Link } from '@astryxdesign/core/Link';
import { SelectField } from './fields/SelectField';
import { TextField } from './fields/TextField';

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
  const groups = useUpstreamGroups();
  if (!groups.available) {
    return <TextField label={label} help={help} placeholder={DEFAULT_GROUP} value={value} onChange={onChange} isDisabled={isDisabled} error={error} />;
  }
  const names = [...groups.names];
  if (value && !names.includes(value)) names.push(value);
  return (
    <SelectField
      label={label}
      value={value}
      onChange={onChange}
      isDisabled={isDisabled}
      emptyLabel={allowEmpty ? `Not set (${DEFAULT_GROUP})` : undefined}
      options={names.map((n) => ({ value: n, label: groups.names.includes(n) ? n : `${n} (not configured)` }))}
      error={error}
      help={
        <>
          {help ? `${help} ` : ''}
          Groups are configured on the <Link href="/upstreams">Upstreams</Link> page.
        </>
      }
    />
  );
}
