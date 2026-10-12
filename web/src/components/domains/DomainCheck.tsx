// "Is <name> allowed for client <ip>?": the Tools dig path (POST
// /dns/query {name, type A, client}) run through the full pipeline, with
// the verdict and the deciding list and rule, so an allow or deny entry can
// be checked right where it was added.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/HStack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useMutation } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { useApi } from '../../api/context';
import { formatError } from '../fields/formats';
import { TextField } from '../fields/TextField';
import { ErrorBanner } from '../Feedback';

export function DomainCheck() {
  const api = useApi();
  const titleId = useId();
  const [name, setName] = useState('');
  const [client, setClient] = useState('');
  const [errors, setErrors] = useState<{ name?: string; client?: string }>({});
  const m = useMutation({
    mutationFn: () => api.runtime.dig({ name: name.trim().replace(/\.$/, ''), type: 'A', ...(client.trim() ? { client: client.trim() } : {}) }),
  });
  const r = m.data;
  const blocked = r?.status_class === 'blocked';
  return (
    <VStack as="section" gap={3} className="k-panel" aria-labelledby={titleId}>
      <VStack gap={1}>
        <Heading level={2} id={titleId}>
          Is it allowed?
        </Heading>
        <Text type="supporting">Asks the resolver as a client would (Tools → Query), with every policy, list and entry applied.</Text>
      </VStack>
      <form
        noValidate
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
        <HStack gap={3} align="end" wrap="wrap">
          <TextField label="Name" mono value={name} onChange={setName} placeholder="ads.example.com" error={errors.name} fieldWidth={280} />
          <TextField label="For client" isOptional mono value={client} onChange={setClient} placeholder="192.168.1.30" error={errors.client} fieldWidth={200} />
          <Button type="submit" label="Check" variant="secondary" isLoading={m.isPending} />
        </HStack>
      </form>
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
    </VStack>
  );
}
