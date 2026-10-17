// "Is <name> allowed for client <ip>?": the Tools dig path (POST
// /dns/query {name, type A, client}) run through the full pipeline, with
// the verdict and the deciding list and rule, so an allow or deny entry can
// be checked right where it was added.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useMutation } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { useApi } from '../../api/context';
import type { DomainEntry } from '../../api/types';
import { refPath, shortRef } from '../../lib/domains';
import { formatError } from '../fields/formats';
import { TextField } from '../fields/TextField';
import { ErrorBanner } from '../Feedback';
import { Panel } from '../layout/Page';

/** Whether a list exception's rule covers the name ("x", "||x^", "*.x" of a parent, "/re/"). */
export function exceptionMatches(e: Pick<DomainEntry, 'value' | 'type'>, name: string): boolean {
  const v = e.value.toLowerCase();
  if (e.type === 'regex' || (v.startsWith('/') && v.endsWith('/') && v.length > 2)) {
    try {
      return new RegExp(v.replace(/^\/(.*)\/$/, '$1')).test(name);
    } catch {
      return false;
    }
  }
  const base = v.startsWith('||') ? v.slice(2).replace(/\^$/, '') : v.startsWith('*.') ? v.slice(2) : v;
  if (base === name) return true;
  return (v.startsWith('||') || v.startsWith('*.')) && name.endsWith(`.${base}`);
}

export function DomainCheck() {
  const api = useApi();
  const titleId = useId();
  const [name, setName] = useState('');
  const [client, setClient] = useState('');
  const [errors, setErrors] = useState<{ name?: string; client?: string }>({});
  const m = useMutation({
    mutationFn: async () => {
      const n = name.trim().replace(/\.$/, '').toLowerCase();
      const r = await api.runtime.dig({ name: n, type: 'A', ...(client.trim() ? { client: client.trim() } : {}) });
      // Entries of lists switched off for this name (list exceptions), so the verdict can say why.
      let exceptions: DomainEntry[] = [];
      try {
        const page = await api.domains.list(undefined, { kind: 'exception', q: n, limit: 100 });
        exceptions = page.items.filter((e) => e.kind === 'exception' && e.enabled && exceptionMatches(e, n));
      } catch {
        // Older servers or no blocklists.read: the verdict alone.
      }
      return { r, exceptions };
    },
  });
  const r = m.data?.r;
  const exceptions = m.data?.exceptions ?? [];
  const blocked = r?.status_class === 'blocked';
  return (
    <Panel title="Is it allowed?" headingId={titleId} description="Asks the resolver as a client would (Tools → Query), with every policy, list and entry applied.">
      <HStack
        as="form"
        gap={3}
        vAlign="end"
        wrap="wrap"
        {...FORM_ATTRS}
        onSubmit={(e) => {
          e.preventDefault();
          const next = {
            ...(name.trim() ? (formatError('domain', name.trim()) ? { name: formatError('domain', name.trim()) ?? '' } : {}) : { name: 'Enter a name' }),
            ...(client.trim() && formatError('ip', client.trim()) ? { client: formatError('ip', client.trim()) ?? '' } : {}),
          };
          setErrors(next);
          if (!next.name && !next.client) m.mutate();
        }}
      >
        <TextField label="Name" mono value={name} onChange={setName} placeholder="ads.example.com" error={errors.name} fieldWidth={280} />
        <TextField label="For client" isOptional mono value={client} onChange={setClient} placeholder="192.168.1.30" error={errors.client} fieldWidth={200} />
        <Button type="submit" label="Check" variant="secondary" isLoading={m.isPending} />
      </HStack>
      {m.error ? <ErrorBanner error={m.error} title="Could not ask the resolver" /> : null}
      {r ? (
        <HStack gap={2} align="center" wrap="wrap" aria-live="polite" data-testid="domain-check-result">
          <Badge variant={blocked ? 'error' : r.status_class === 'error' ? 'warning' : 'success'} label={blocked ? 'blocked' : r.status_class === 'error' ? 'error' : 'allowed'} />
          <Text type="code">{r.name}</Text>
          {r.client ? <Text type="supporting">for {r.client}</Text> : null}
          <Text type="supporting">{r.status.replace(/_/g, ' ')}</Text>
          {r.list ? <Text type="supporting">list {r.list}</Text> : null}
          {r.rule ? (
            <Text type="supporting">
              rule <Text type="code">{r.rule}</Text>
            </Text>
          ) : null}
        </HStack>
      ) : null}
      {r && exceptions.length ? (
        <VStack gap={1} data-testid="domain-check-exceptions">
          {exceptions.map((e) => (
            <Text key={e.id} type="supporting">
              <Text type="code">{e.value}</Text> is switched off in{' '}
              <Link href={refPath('lists', e.lists[0]?.list ?? '')} hasUnderline>
                {shortRef(e.lists[0]?.list ?? '')}
              </Link>{' '}
              (a list exception): that list {e.action === 'allow' ? 'does not block' : 'does not allow'} it
              {e.stillMatchedBy.length ? `; still ${e.action === 'allow' ? 'blocked' : 'allowed'} by ${e.stillMatchedBy.map((o) => shortRef(o.list)).join(', ')}` : ''}.
            </Text>
          ))}
        </VStack>
      ) : null}
    </Panel>
  );
}

/** <form> attributes the Astryx stack props do not type (they reach the element). */
const FORM_ATTRS: object = { noValidate: true };
