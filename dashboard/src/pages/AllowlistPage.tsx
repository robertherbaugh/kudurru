// The Allowlist (/allowlist): every allow entry, for everyone or for some
// client groups (components/domains/DomainEntriesView).
import { DomainEntriesView } from '../components/domains/DomainEntriesView';

export default function AllowlistPage() {
  return <DomainEntriesView action="allow" />;
}
