import { Button } from '@astryxdesign/core/Button';
import { List, ListItem } from '@astryxdesign/core/List';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useEffect, useRef, useState } from 'react';
import { useApi } from '../../api/context';
import type { DomainEntry, DomainRegexImpact } from '../../api/types';
import { refPath, shortRef } from '../../lib/domains';
import { TextField } from '../fields/TextField';
import { FormDialog } from '../FormDialog';

/** Mount a new instance for each inspection so a closed request cannot return into another dialog. */
export function RegexImpactDialog({ entry, onClose }: { entry: DomainEntry; onClose: () => void }) {
  const api = useApi();
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<DomainRegexImpact>();
  const [error, setError] = useState('');
  const active = useRef(false);
  useEffect(() => {
    active.current = true;
    return () => { active.current = false; };
  }, []);
  const check = async () => {
    if (busy || !name.trim()) return;
    setBusy(true);
    setError('');
    setResult(undefined);
    try {
      const response = await api.domains.impact(entry.action, entry.id, name.trim());
      if (active.current) setResult(response);
    } catch (cause) {
      if (active.current) setError(cause instanceof Error ? cause.message : 'Could not inspect this expression.');
    } finally {
      if (active.current) setBusy(false);
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title="Test a domain against this regex"
      onSubmit={() => void check()}
      footer={<><Button label="Done" onClick={onClose} /><Button label="Check domain" type="submit" variant="primary" isLoading={busy} isDisabled={!name.trim()} /></>}
    >
      <Text type="code" wordBreak="break-all">{result?.expression ?? entry.value}</Text>
      <TextField
        label="Domain"
        help="Enter one domain, such as ads.example.com. No DNS query is sent."
        value={name}
        isDisabled={busy}
        onChange={(value) => { setName(value); setResult(undefined); setError(''); }}
        error={error || undefined}
      />
      <Text type="supporting">Checks one name and opposing lists for an A/IN query without a client identity. Policies, schedules, modifiers and other rules still determine the DNS result.</Text>
      {result ? (
        <VStack gap={2} aria-live="polite">
          <Text>{result.name} {result.matches ? 'matches this expression.' : 'does not match this expression.'}</Text>
          {!result.enabled ? <Text>This entry is disabled and has no effect.</Text> : null}
          <Text type="supporting">Scope: {result.everyone ? 'Everyone' : result.groups.map((group) => shortRef(group, entry.namespace)).join(', ') || 'No client groups'}.</Text>
          {result.overlaps.length ? (
            <List density="compact" header={result.action === 'allow' ? 'Matching block lists this allow entry may override:' : 'Matching allow lists that may override this deny entry:'}>
              {result.overlaps.map((overlap) => (
                <ListItem key={overlap.list} label={shortRef(overlap.list, entry.namespace)} href={refPath('lists', overlap.list)} description={<Text type="code" wordBreak="break-all">{overlap.rule}</Text>} />
              ))}
            </List>
          ) : result.matches ? <Text>No opposing list matches were found in the inspected lists.</Text> : null}
          {result.incomplete ? <Text>Partial results: some lists or matches exceeded the inspection limits.</Text> : null}
        </VStack>
      ) : null}
    </FormDialog>
  );
}
