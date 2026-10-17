// Badges for states, as the dashboard shows them: a license's state in the
// catalog's tone (only states that need action are loud), roles, member and
// request states. One place, so every table and header words a state the
// same way.
import { Badge } from '@astryxdesign/core/Badge';
import type { BusinessRequestStatus, MemberStatus, RequestStatus } from '@shared/api';
import { stateLabel, stateTone, type LicenseState } from '@shared/catalog';
import { roleLabel, type Role } from '@shared/permissions';
import { BUSINESS_REQUEST_STATUS_LABEL, MEMBER_STATUS_LABEL, REQUEST_STATUS_LABEL } from '../lib/words';

export function StateBadge({ state }: { state: LicenseState }) {
  return <Badge variant={stateTone(state)} label={stateLabel(state)} />;
}

export function RoleBadges({ roles, empty = 'No role' }: { roles: readonly Role[]; empty?: string }) {
  if (!roles.length) return <Badge variant="neutral" label={empty} />;
  return (
    <>
      {roles.map((r) => (
        <Badge key={r} variant={r === 'customer' ? 'neutral' : 'info'} label={roleLabel(r)} />
      ))}
    </>
  );
}

export function MemberStatusBadge({ status }: { status: MemberStatus }) {
  return <Badge variant={status === 'linked' ? 'success' : 'neutral'} label={MEMBER_STATUS_LABEL[status]} />;
}

export function RequestStatusBadge({ status }: { status: RequestStatus }) {
  const variant = status === 'open' ? 'warning' : status === 'approved' ? 'success' : 'neutral';
  return <Badge variant={variant} label={REQUEST_STATUS_LABEL[status]} />;
}

export function BusinessRequestStatusBadge({ status }: { status: BusinessRequestStatus }) {
  const variant = status === 'open' ? 'warning' : status === 'issued' ? 'success' : 'neutral';
  return <Badge variant={variant} label={BUSINESS_REQUEST_STATUS_LABEL[status]} />;
}
