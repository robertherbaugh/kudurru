// Appearance: auto/light/dark mode, the Astryx theme (neutral, stone and
// the fun Y2K theme) and the high-contrast overlay (§10 principle 4).
import { Button } from '@astryxdesign/core/Button';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { Switch } from '@astryxdesign/core/Switch';
import { Palette } from 'lucide-react';
import { VStack } from '@astryxdesign/core/VStack';
import { useState } from 'react';
import { SelectField } from './fields/SelectField';
import { FormDialog } from './FormDialog';
import { THEMES, useAppearance, type Mode, type ThemeName } from '../lib/theme';

export function AppearanceControls() {
  const a = useAppearance();
  return (
    <VStack gap={4}>
      <SegmentedControl label="Mode" value={a.mode} onChange={(v) => a.setMode(v as Mode)}>
        <SegmentedControlItem value="system" label="Auto" />
        <SegmentedControlItem value="light" label="Light" />
        <SegmentedControlItem value="dark" label="Dark" />
      </SegmentedControl>
      <SelectField label="Theme" value={a.theme} options={THEMES.map((t) => ({ value: t.id, label: t.label }))} onChange={(v) => a.setTheme(v as ThemeName)} />
      <Switch
        label="High contrast"
        description="Stronger status colours and borders"
        value={a.contrast === 'more'}
        onChange={(on) => a.setContrast(on ? 'more' : 'normal')}
      />
    </VStack>
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
      <FormDialog isOpen={open} onClose={() => setOpen(false)} title="Appearance" width={380} footer={<Button label="Done" variant="primary" onClick={() => setOpen(false)} />}>
        <AppearanceControls />
      </FormDialog>
    </>
  );
}
