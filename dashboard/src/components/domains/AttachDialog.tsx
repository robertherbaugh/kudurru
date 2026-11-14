// "Attach" for a policy the dashboard may not change (`unattached` on the
// Allowlist and Denylist, D143): a policy managed by a file, by Kubernetes
// or by an integration should reference a ui-allow / ui-deny list but does
// not, so the entries of that list do not apply to its clients. The dialog
// shows the exact YAML to put in the policy's own definition (the list
// added to spec.allowListRefs or spec.blockListRefs, merged with the refs
// the policy already has when GET /objects/policies/{ns}/{name} can read
// it) and says where that definition lives from the policy's
// kudurru.io/managed-by value: a file in the objects directory (watched and
// re-applied on change), the objects: of the configuration file (also watched),
// a Kubernetes resource (kubectl or GitOps) or an integration.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { ApiError } from '../../api/client';
import { qk, useApi } from '../../api/context';
import type { DomainUnattached, Policy } from '../../api/types';
import { refField, shortRef, uiListAction } from '../../lib/domains';
import { ownership } from '../../lib/managedBy';
import { toYAML } from '../../lib/yaml';
import { Loading } from '../Feedback';
import { FormDialog } from '../FormDialog';

/** "ns/name" → [ns, name] ("default" when bare). */
function splitRef(ref: string): [string, string] {
  const i = ref.indexOf('/');
  return i > 0 ? [ref.slice(0, i), ref.slice(i + 1)] : ['default', ref];
}

export interface AttachPlan {
  /** The policy field the list belongs in. */
  field: 'allowListRefs' | 'blockListRefs';
  /** The list as the policy names it (bare in the policy's own namespace). */
  ref: string;
  /** The whole field after the change; null when the policy could not be read. */
  refs: string[] | null;
  /** The policy lists the ref already (it was changed since the page loaded). */
  present: boolean;
  /** The YAML to put under the policy's definition. */
  yaml: string;
  /** 1-based line of the added ref in `yaml`. */
  line: number;
  /** A kubectl merge patch setting the field (only with the whole field). */
  kubectl: string | null;
}

/**
 * What to add to a policy so it uses the list: spec.<field> with the list
 * appended to the refs the policy already has (when it was read), else the
 * new ref under a comment that keeps the existing ones.
 */
export function attachPlan(u: Pick<DomainUnattached, 'list' | 'policy'>, policy?: Policy | null): AttachPlan {
  const [listNs, listName] = splitRef(u.list);
  const [policyNs, policyName] = splitRef(u.policy);
  const field = refField(uiListAction(listName) ?? 'allow');
  // The server matches a ui list by its bare name in the policy's namespace (D143).
  const ref = listNs === policyNs ? listName : `${listNs}/${listName}`;
  if (!policy) {
    const yaml = `spec:\n  ${field}:\n  # keep the entries the policy already has here, then add:\n  - ${ref}\n`;
    return { field, ref, refs: null, present: false, yaml, line: 4, kubectl: null };
  }
  const current = policy.spec[field] ?? [];
  const present = current.includes(ref) || current.includes(`${listNs}/${listName}`);
  const refs = present ? current : [...current, ref];
  const yaml = toYAML({ spec: { [field]: refs } });
  const line = 3 + refs.indexOf(ref);
  const patch = JSON.stringify({ spec: { [field]: refs } });
  return { field, ref, refs, present, yaml, line, kubectl: `kubectl -n ${policyNs} patch policies.kudurru.io ${policyName} --type merge -p '${patch}'` };
}

/** Where the policy's definition lives and how a change reaches kudurru, from its kudurru.io/managed-by value. */
export function attachWhere(managedBy: string, policy: string, field: string): { lead: ReactNode; after: string } {
  const [ns, name] = splitRef(policy);
  const o = ownership(managedBy);
  const spec = <Code size="inherit">spec.{field}</Code>;
  switch (o.kind) {
    case 'file': {
      const path = o.owner.slice('file:'.length);
      // "objects.d/<file>": a file of the watched objects directory; a bare name: objects: of the configuration file.
      if (path.includes('/')) {
        return {
          lead: (
            <>
              {name} is defined in <Code size="inherit">{path}</Code> on the kudurru host, in the objects directory (<Code size="inherit">objectsDir</Code>, <Code size="inherit">/etc/kudurru/objects.d</Code> by default). In
              its <Code size="inherit">kind: Policy</Code> document named {name}, set {spec} to:
            </>
          ),
          after: 'kudurru watches the directory and applies the saved file within seconds, without a restart. With several nodes sharing copies of the files, change the file on every node.',
        };
      }
      return {
        lead: (
          <>
            {name} is defined under <Code size="inherit">objects:</Code> in <Code size="inherit">{path}</Code>, the kudurru configuration file on the host. In that <Code size="inherit">kind: Policy</Code> entry, set {spec} to:
          </>
        ),
        after: 'kudurru watches the configuration file and applies valid objects without a restart. Invalid input keeps the previous objects. Change the file on every node that uses a copy.',
      };
    }
    case 'kubernetes':
      return {
        lead: (
          <>
            {name} is a Policy resource in Kubernetes (namespace {ns}). In its manifest, or with <Code size="inherit">kubectl edit policies.kudurru.io {name} -n {ns}</Code>, set {spec} to:
          </>
        ),
        after: 'If a GitOps tool applies the manifest, change it in the repository, or the next sync undoes the edit.',
      };
    case 'integration':
      return {
        lead: (
          <>
            {name} is written by the {o.label} integration, which would undo a change made anywhere else. Where that integration takes its policies from, set {spec} to:
          </>
        ),
        after: 'The change reaches kudurru at the integration’s next sync.',
      };
    default:
      return {
        lead: (
          <>
            {name} is managed by <Code size="inherit">{o.owner || 'nobody'}</Code>. Where it is defined, set {spec} to:
          </>
        ),
        after: '',
      };
  }
}

export interface AttachDialogProps {
  unattached: DomainUnattached | null;
  onClose: () => void;
}

export function AttachDialog({ unattached: u, onClose }: AttachDialogProps) {
  const api = useApi();
  const [policyNs, policyName] = splitRef(u?.policy ?? '');
  // The policy as it is now, so the snippet keeps its other refs; without policies.read the snippet says to keep them.
  const q = useQuery({
    queryKey: [...qk.objects('policies'), policyNs, policyName],
    queryFn: () => api.objects.get<Policy>('policies', policyName, policyNs),
    enabled: !!u,
    retry: false,
  });
  const plan = u && !q.isLoading ? attachPlan(u, q.data) : null;
  const where = u && plan ? attachWhere(u.managedBy, u.policy, plan.field) : null;
  return (
    <FormDialog
      isOpen={!!u}
      onClose={onClose}
      title={u ? `Attach ${shortRef(u.list)} to ${shortRef(u.policy)}` : 'Attach'}
      subtitle={u ? `Managed by ${u.managedBy}` : undefined}
      width={640}
      footer={<Button label="Close" variant="primary" onClick={onClose} />}
    >
      {!u ? null : !plan || !where ? (
        <Loading label="Reading the policy" />
      ) : (
        <VStack gap={3}>
          <Text as="p">
            The dashboard does not change {shortRef(u.policy)}, so the entries of {shortRef(u.list)} do not apply to its clients until the policy lists it. {where.lead}
          </Text>
          {plan.present ? <Banner status="info" title={`${shortRef(u.policy)} lists ${plan.ref} already`} description="Its definition was changed after this page loaded; the warning goes away when the page refreshes." /> : null}
          <CodeBlock code={plan.yaml} language="yaml" title={`Policy ${shortRef(u.policy)}`} highlightLines={plan.present ? undefined : [plan.line]} width="100%" data-testid="attach-yaml" />
          {plan.refs ? (
            <Text type="supporting" as="p">
              The other fields of the policy stay as they are; {plan.field} keeps the lists it has and gains {plan.ref}.
            </Text>
          ) : (
            <Text type="supporting" as="p">
              {q.error instanceof ApiError && q.error.status === 403 ? 'Reading the policy needs policies.read' : 'The policy could not be read'}, so the lists it has in {plan.field} are not shown: keep them and add {plan.ref}.
            </Text>
          )}
          {ownership(u.managedBy).kind === 'kubernetes' && plan.kubectl ? (
            <>
              <Text as="p">Or patch it in place:</Text>
              <CodeBlock code={plan.kubectl} language="bash" isWrapped width="100%" data-testid="attach-kubectl" />
            </>
          ) : null}
          {where.after ? <Text as="p">{where.after}</Text> : null}
        </VStack>
      )}
    </FormDialog>
  );
}
