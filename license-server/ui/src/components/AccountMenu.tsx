// The signed-in person in the top bar: a Popover with their name, verified
// email and roles, Appearance and Sign out (POST /api/v1/auth/logout). The
// popover closes before the Appearance dialog opens, so no overlay sits on
// another.
import { Button } from '@astryxdesign/core/Button';
import { Divider } from '@astryxdesign/core/Divider';
import { Popover } from '@astryxdesign/core/Popover';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { LogOut, Palette, UserRound } from 'lucide-react';
import { useState } from 'react';
import { useSession, useSignOut } from '../api/context';
import { AppearanceDialog } from '../ui/components/AppearanceButton';
import { RoleBadges } from './badges';

export function AccountMenu() {
  const { data: me } = useSession();
  const signOut = useSignOut();
  const [open, setOpen] = useState(false);
  const [appearance, setAppearance] = useState(false);
  const [leaving, setLeaving] = useState(false);
  if (!me) return null;
  const content = (
    <VStack gap={3} width={280}>
      <VStack gap={1}>
        <Text weight="semibold">{me.user.name || me.user.email || 'Signed in'}</Text>
        <Text type="supporting">{me.user.email ?? 'No verified email address'}</Text>
        <HStack gap={1} wrap="wrap">
          <RoleBadges roles={me.roles} />
        </HStack>
      </VStack>
      <Divider />
      <VStack gap={1}>
        <Button
          label="Appearance"
          variant="ghost"
          icon={<Palette size={16} aria-hidden="true" />}
          width="100%"
          onClick={() => {
            setOpen(false);
            setAppearance(true);
          }}
        />
        <Button
          label="Sign out"
          variant="ghost"
          icon={<LogOut size={16} aria-hidden="true" />}
          width="100%"
          isLoading={leaving}
          clickAction={async () => {
            setLeaving(true);
            await signOut();
          }}
        />
      </VStack>
    </VStack>
  );
  return (
    <>
      <Popover label="Account" content={content} isOpen={open} onOpenChange={setOpen} placement="below" alignment="end">
        <Button label={me.user.name || me.user.email || 'Account'} variant="ghost" size="sm" icon={<UserRound size={16} aria-hidden="true" />} />
      </Popover>
      <AppearanceDialog isOpen={appearance} onClose={() => setAppearance(false)} />
    </>
  );
}
