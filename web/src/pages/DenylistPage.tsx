// The Denylist (/denylist): every deny entry, for everyone or for some
// client groups (components/domains/DomainEntriesView).
import { DomainEntriesView } from '../components/domains/DomainEntriesView';

export default function DenylistPage() {
  return <DomainEntriesView action="deny" />;
}
