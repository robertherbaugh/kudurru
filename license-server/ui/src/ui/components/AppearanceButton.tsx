// Copied from web/src/components/AppearanceButton.tsx at dd3dcb0; keep in step by hand.
// Changes: the portal has one theme (kudurru), so the theme picker is gone;
// AppearanceDialog is exported for the account menu, which opens it after
// closing its popover (no nested overlays).
//
// Appearance: auto/light/dark mode and high contrast.
import { Button } from '@astryxdesign/core/Button';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { Switch } from '@astryxdesign/core/Switch';
import { Palette } from 'lucide-react';
import { VStack } from '@astryxdesign/core/VStack';
import { useState } from 'react';
import { FormDialog } from './FormDialog';
import { useAppearance, type Mode } from '../lib/theme';

export function AppearanceControls() {
  const a = useAppearance();
  return (
    <VStack gap={4}>
      <SegmentedControl label="Mode" value={a.mode} onChange={(v) => a.setMode(v as Mode)}>
        <SegmentedControlItem value="system" label="Auto" />
        <SegmentedControlItem value="light" label="Light" />
        <SegmentedControlItem value="dark" label="Dark" />
      </SegmentedControl>
      <Switch
        label="High contrast"
        description="Stronger borders and secondary text"
        value={a.contrast === 'more'}
        onChange={(on) => a.setContrast(on ? 'more' : 'normal')}
      />
    </VStack>
  );
}

export function AppearanceDialog({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Appearance" width={380} footer={<Button label="Done" variant="primary" onClick={onClose} />}>
      <AppearanceControls />
    </FormDialog>
  );
}

export function AppearanceButton() {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button
        label="Appearance"
        variant="ghost"
        size="sm"
        isIconOnly
        icon={<Palette size={16} aria-hidden="true" />}
        tooltip="Appearance"
        onClick={() => setOpen(true)}
      />
      <AppearanceDialog isOpen={open} onClose={() => setOpen(false)} />
    </>
  );
}
