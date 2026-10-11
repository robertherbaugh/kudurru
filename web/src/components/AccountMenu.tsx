// The signed-in principal and the sign-out action.
import { Button } from '@astryxdesign/core/Button';
import { LogOut } from 'lucide-react';
import { useSession, useSignOut } from '../api/context';
import { Link } from '../lib/router';

export function AccountMenu() {
  const { data } = useSession();
  const signOut = useSignOut();
  if (!data) return null;
  return (
    <span className="k-row k-account">
      <Link href="/access" className="k-account-name" title={`${data.principal.kind} ${data.principal.id}; roles: ${data.roles.join(', ') || 'none'}`}>
        {data.principal.name}
      </Link>
      <Button
        label="Sign out"
        variant="ghost"
        size="sm"
        isIconOnly
        icon={<LogOut size={16} aria-hidden="true" />}
        tooltip="Sign out"
        onClick={() => void signOut()}
      />
    </span>
  );
}
