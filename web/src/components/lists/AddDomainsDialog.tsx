// "Add domains": exact or regex entries into the ui-allow / ui-deny lists
// (POST /domains/{allow|deny}/{exact|regex}, processed{success,errors}). The
// server creates the list on demand and references it from Policy
// <namespace>/default when the API owns that policy. Shared by the Lists
// and Domains pages.
import { Button } from '@astryxdesign/core/Button';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../../api/context';
import type { Processed } from '../../api/types';
import { EnumSelect, type EnumOption } from '../fields/EnumSelect';
import { TextAreaField } from '../fields/TextField';
import { useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';

export type DomainAction = 'deny' | 'allow';
export type DomainKind = 'exact' | 'regex';
export type DomainType = `${DomainAction}:${DomainKind}`;

export const DOMAIN_TYPES: (EnumOption & { value: DomainType })[] = [
  { value: 'deny:exact', label: 'Deny: exact domain', help: 'Blocks this name. "*.example.com" blocks example.com and every subdomain.' },
  { value: 'deny:regex', label: 'Deny: regular expression', help: 'Blocks every name the RE2 expression matches, e.g. ^ads?[0-9]*\\.' },
  { value: 'allow:exact', label: 'Allow: exact domain', help: 'Never blocks this name, whatever the block lists say. "*.example.com" includes subdomains.' },
  { value: 'allow:regex', label: 'Allow: regular expression', help: 'Never blocks names the RE2 expression matches.' },
];

export function splitType(t: DomainType): [DomainAction, DomainKind] {
  return t.split(':') as [DomainAction, DomainKind];
}

/** The UI list a domain type writes to. */
export function uiListOf(action: DomainAction): string {
  return action === 'allow' ? 'ui-allow' : 'ui-deny';
}

// internal/api dnsName: an optional "*." and RFC 1123-ish labels (underscores allowed).
const DNS_NAME = /^(\*\.)?([a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?\.)*[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?$/;

/** Mirrors the server's domainEntry checks so mistakes show before sending. */
export function domainError(kind: DomainKind, d: string): string | null {
  const v = d.trim();
  if (!v) return 'Required';
  if (kind === 'regex') {
    const re = v.replace(/^\//, '').replace(/\/$/, '');
    if (!re) return 'Empty expression';
    try {
      new RegExp(re);
      return null;
    } catch (e) {
      return `Not a valid expression: ${(e as Error).message}`;
    }
  }
  const n = v.toLowerCase().replace(/\.$/, '');
  if (n.length > 253 || !DNS_NAME.test(n)) return `"${v}" is not a domain name (example: ads.example.com)`;
  return null;
}

/** Domains of the text box: one per line, or separated by spaces or commas. */
export function splitDomains(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

export function AddDomainsDialog({ open, onClose, defaultType = 'deny:exact' }: { open: boolean; onClose: () => void; defaultType?: DomainType }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const [type, setType] = useState<DomainType>(defaultType);
  const [text, setText] = useState('');
  const [result, setResult] = useState<Processed['processed'] | null>(null);
  const [action, kind] = splitType(type);
  const domains = splitDomains(text);
  const firstError = domains.map((d) => [d, domainError(kind, d)] as const).find(([, e]) => e);
  const m = useMutation({
    mutationFn: () => api.lists.addDomains(action, kind, domains),
    onSuccess: (r) => {
      setResult(r.processed);
      if (r.processed.success.length) notify.success(`Added ${r.processed.success.length} to ${uiListOf(action)}`);
      if (!r.processed.errors.length) setText('');
      void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
      void qc.invalidateQueries({ queryKey: qk.objects('policies') });
    },
    onError: (err) => notify.error(err, 'Could not add domains'),
  });
  const close = () => {
    setResult(null);
    onClose();
  };
  return (
    <FormDialog
      isOpen={open}
      onClose={close}
      title="Add domains"
      width={560}
      onSubmit={() => {
        if (domains.length && !firstError) m.mutate();
      }}
      footer={
        <>
          <Button label="Close" variant="ghost" onClick={close} />
          <Button type="submit" label={`Add ${domains.length || ''}`.trim()} variant="primary" isDisabled={!domains.length || !!firstError} isLoading={m.isPending} />
        </>
      }
    >
      <EnumSelect
        label="Type"
        value={type}
        options={DOMAIN_TYPES}
        onChange={(v) => {
          setType(v as DomainType);
          setResult(null);
        }}
      />
      <TextAreaField
        label={kind === 'regex' ? 'Expressions' : 'Domains'}
        help={`One per line, or separated by spaces. They go into the "${uiListOf(action)}" list (created on demand and used by the default policy).`}
        placeholder={kind === 'regex' ? '^ads?[0-9]*\\.\n(^|\\.)doubleclick\\.net$' : 'ads.example.com\n*.tracker.example'}
        value={text}
        onChange={(v) => {
          setText(v);
          setResult(null);
        }}
        rows={5}
        hasSpellCheck={false}
        error={firstError ? `${firstError[0]}: ${firstError[1]}` : undefined}
      />
      {result ? (
        <div className="k-stack" role="status">
          <p>
            {result.success.length} added, {result.errors.length} rejected.
          </p>
          {result.errors.length ? (
            <ul className="k-small">
              {result.errors.map((e) => (
                <li key={e.item}>
                  <code>{e.item}</code>: {e.error}
                </li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}
    </FormDialog>
  );
}
