// The members of a ClientGroup (spec.clients) as a typed editor: one row
// per matcher with its kind (IP address, CIDR range, client ID, MAC
// address), the value checked for that kind, a display name and a comment.
// Exactly one selector field is set per matcher, as the CRD requires.
// Hostname and name-pattern matchers of existing groups stay editable but
// say that this version has no hostname source, so they never match.
import { Button } from '@astryxdesign/core/Button';
import { TextArea } from '@astryxdesign/core/TextArea';
import { useId, useState } from 'react';
import type { ClientItem, ClientMatcher } from '../../api/types';
import { cidrError, ipError, macError } from '../../lib/objectSchemas';
import { EnumSelect, type EnumOption } from './EnumSelect';

export type MatcherKind = 'ip' | 'cidr' | 'clientID' | 'mac' | 'hostname' | 'nameGlob';

const KINDS: (EnumOption & { value: MatcherKind; placeholder: string })[] = [
  { value: 'ip', label: 'IP address', placeholder: '192.168.1.30 or 2001:db8::30', help: 'One device by its address.' },
  { value: 'cidr', label: 'CIDR range', placeholder: '192.168.1.0/24', help: 'Every address in the range; the longest matching range wins.' },
  { value: 'clientID', label: 'Client ID', placeholder: 'kids-tablet', help: 'The DoT server name label or DoH path ID the device connects with.' },
  { value: 'mac', label: 'MAC address', placeholder: 'aa:bb:cc:dd:ee:ff', help: 'Matches only when the transport supplies MAC addresses; inventory integrations arrive in v0.2.' },
  { value: 'hostname', label: 'Host name', placeholder: 'tablet.home.arpa', help: 'Needs a hostname source; this version has none, so it never matches.' },
  { value: 'nameGlob', label: 'Name pattern', placeholder: 'kids-*', help: 'Needs a hostname source; this version has none, so it never matches.' },
];

const OFFERED: MatcherKind[] = ['ip', 'cidr', 'clientID', 'mac'];

export function matcherKind(m: ClientMatcher): MatcherKind {
  if (m.ip !== undefined) return 'ip';
  if (m.cidr !== undefined) return 'cidr';
  if (m.clientID !== undefined) return 'clientID';
  if (m.mac !== undefined) return 'mac';
  if (m.hostname !== undefined) return 'hostname';
  if (m.nameGlob !== undefined) return 'nameGlob';
  return 'ip';
}

export function matcherValue(m: ClientMatcher): string {
  return m[matcherKind(m)] ?? '';
}

export function kindLabel(k: MatcherKind): string {
  return KINDS.find((x) => x.value === k)?.label ?? k;
}

/** "kids-tablet (192.168.1.30)" style. */
export function describeMatcher(m: ClientMatcher): string {
  const v = matcherValue(m) || '?';
  const what = matcherKind(m) === 'clientID' ? `id ${v}` : v;
  return m.name ? `${m.name} (${what})` : what;
}

/** A copy with the selector set to kind = value and every other selector cleared. */
export function withSelector(m: ClientMatcher, kind: MatcherKind, value: string): ClientMatcher {
  const { ip: _ip, cidr: _cidr, clientID: _id, mac: _mac, hostname: _h, nameGlob: _g, ...rest } = m;
  return { [kind]: value, ...rest };
}

const CLIENT_ID = /^[A-Za-z0-9]([A-Za-z0-9._-]{0,62})$/;
const HOST = /^([A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)*[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.?$/;

/** The message for a matcher's value, null when valid. */
export function matcherError(m: ClientMatcher): string | null {
  const kind = matcherKind(m);
  const v = matcherValue(m).trim();
  if (!v) return 'Required';
  switch (kind) {
    case 'ip':
      return ipError(v);
    case 'cidr':
      return cidrError(v);
    case 'mac':
      return macError(v);
    case 'clientID':
      return CLIENT_ID.test(v) ? null : 'Letters, digits, ".", "_" and "-", at most 63 characters';
    case 'hostname':
      return HOST.test(v) ? null : 'A host name such as tablet.home.arpa';
    case 'nameGlob':
      return /^[A-Za-z0-9_.*?[\]-]+$/.test(v) ? null : 'A pattern such as kids-* (letters, digits, ".", "-", * and ?)';
  }
}

/** The first invalid matcher of a list, as "Matcher 2: …", or null. */
export function matchersError(ms: ClientMatcher[]): string | null {
  const bad = ms.map((m, i) => [i, matcherError(m)] as const).filter(([, e]) => e);
  if (!bad.length) return null;
  const [i, e] = bad[0] as readonly [number, string];
  return `Matcher ${i + 1}: ${e}${bad.length > 1 ? ` (and ${bad.length - 1} more)` : ''}`;
}

/** The matcher that selects a client of GET /clients by its key (an IP address or a client ID). */
export function matcherFor(c: ClientItem | { key: string; name?: string | undefined }, comment: string): ClientMatcher {
  const m: ClientMatcher = ipError(c.key) === null ? { ip: c.key } : { clientID: c.key };
  if (c.name) m.name = c.name;
  if (comment) m.comment = comment;
  return m;
}

/** The matcher a pasted line becomes: a range, a MAC, an address or a client ID. */
export function matcherFromText(line: string): ClientMatcher {
  const t = line.trim();
  if (t.includes('/')) return { cidr: t };
  if (/^([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$/i.test(t)) return { mac: t.toLowerCase() };
  if (!ipError(t)) return { ip: t };
  return { clientID: t };
}

export interface MatchersFieldProps {
  value: ClientMatcher[];
  onChange: (v: ClientMatcher[]) => void;
  isDisabled?: boolean;
  /** Show every row's validation message (after a submit attempt). */
  showErrors?: boolean;
}

export function MatchersField({ value, onChange, isDisabled, showErrors }: MatchersFieldProps) {
  const id = useId();
  const [bulk, setBulk] = useState<string | null>(null);
  const update = (i: number, m: ClientMatcher) => onChange(value.map((x, j) => (j === i ? m : x)));
  const lines = (bulk ?? '').split(/[\r\n,]+/).map((s) => s.trim()).filter(Boolean);
  return (
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend>Members</legend>
      <p className="k-muted k-small">A client is in the group when any one matcher matches it. Exact addresses beat ranges, ranges beat client IDs.</p>
      {value.length === 0 ? <p className="k-muted k-small">No matchers yet: the group matches nobody.</p> : null}
      <ol className="k-matchers" aria-label="Matchers">
        {value.map((m, i) => {
          const kind = matcherKind(m);
          const def = KINDS.find((k) => k.value === kind);
          const err = matcherError(m);
          const touched = showErrors || matcherValue(m) !== '';
          const options = KINDS.filter((k) => OFFERED.includes(k.value) || k.value === kind);
          return (
            <li key={i} className="k-matcher" data-testid={`matcher-${i}`}>
              <EnumSelect label={`Matcher ${i + 1} kind`} value={kind} options={options} onChange={(k) => update(i, withSelector(m, k as MatcherKind, matcherValue(m)))} isDisabled={isDisabled} />
              <label className="k-label" htmlFor={`${id}-v${i}`}>
                {def?.label ?? 'Value'}
                <input
                  id={`${id}-v${i}`}
                  className="k-native-input k-mono"
                  value={matcherValue(m)}
                  placeholder={def?.placeholder}
                  aria-invalid={touched && err ? true : undefined}
                  aria-label={`Matcher ${i + 1} ${def?.label ?? 'value'}`}
                  onChange={(e) => update(i, withSelector(m, kind, e.target.value))}
                  spellCheck={false}
                />
                {touched && err ? <span className="k-error-text">{err}</span> : null}
              </label>
              <label className="k-label" htmlFor={`${id}-n${i}`}>
                Display name
                <input id={`${id}-n${i}`} className="k-native-input" value={m.name ?? ''} placeholder="kids-tablet" aria-label={`Matcher ${i + 1} display name`} onChange={(e) => update(i, { ...m, name: e.target.value || undefined })} />
              </label>
              <label className="k-label" htmlFor={`${id}-c${i}`}>
                Comment
                <input id={`${id}-c${i}`} className="k-native-input" value={m.comment ?? ''} aria-label={`Matcher ${i + 1} comment`} onChange={(e) => update(i, { ...m, comment: e.target.value || undefined })} />
              </label>
              <Button label="Remove" size="sm" variant="ghost" isDisabled={isDisabled} onClick={() => onChange(value.filter((_, j) => j !== i))} tooltip={`Remove matcher ${i + 1}`} />
            </li>
          );
        })}
      </ol>
      {isDisabled ? null : (
        <div className="k-row">
          <Button label="Add matcher" size="sm" variant="secondary" onClick={() => onChange([...value, { ip: '' }])} />
          <Button label={bulk === null ? 'Paste many' : 'Close paste'} size="sm" variant="ghost" onClick={() => setBulk(bulk === null ? '' : null)} />
        </div>
      )}
      {bulk !== null ? (
        <div className="k-stack">
          <TextArea label="Paste addresses, ranges, MAC addresses or client IDs" description="One per line or comma separated; each line becomes a matcher of the kind it looks like." value={bulk} onChange={setBulk} rows={4} hasSpellCheck={false} />
          <div className="k-row">
            <Button
              label={`Add ${lines.length || ''} matchers`.replace('  ', ' ')}
              size="sm"
              variant="primary"
              isDisabled={lines.length === 0}
              onClick={() => {
                onChange([...value, ...lines.map(matcherFromText)]);
                setBulk(null);
              }}
            />
          </div>
        </div>
      ) : null}
    </fieldset>
  );
}

/** The matchers with empty optional fields dropped and values trimmed. */
export function cleanMatchers(ms: ClientMatcher[]): ClientMatcher[] {
  return ms.map((m) => {
    const kind = matcherKind(m);
    const out: ClientMatcher = { [kind]: matcherValue(m).trim() };
    if (m.name?.trim()) out.name = m.name.trim();
    if (m.comment?.trim()) out.comment = m.comment.trim();
    return out;
  });
}
