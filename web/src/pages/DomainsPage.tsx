// Domains (/domains, like Pi-hole's Domains page): the allow and deny
// entries together, filterable by action, type, scope, state and text,
// with links to the focused Allowlist and Denylist pages. The same table,
// row actions and dialogs as those (components/domains/DomainEntriesView).
import { DomainEntriesView } from '../components/domains/DomainEntriesView';

export default function DomainsPage() {
  return <DomainEntriesView />;
}
