import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { Table } from '@astryxdesign/core/Table';
import { Dialog } from '@astryxdesign/core/Dialog';
import { Selector } from '@astryxdesign/core/Selector';
import { Switch } from '@astryxdesign/core/Switch';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { useToast } from '@astryxdesign/core/Toast';
import { LayerProvider } from '@astryxdesign/core/Layer';

function Demo() {
  const [v, setV] = useState('');
  const [open, setOpen] = useState(false);
  const [sel, setSel] = useState('a');
  const [sw, setSw] = useState(false);
  const [seg, setSeg] = useState('x');
  const toast = useToast();
  return (
    <div>
      <TextInput label="Name" value={v} onChange={setV} />
      <Button label="Open" onClick={() => setOpen(true)} />
      <Button label="Toast" onClick={() => toast({ body: 'hello toast' })} />
      <Dialog isOpen={open} onOpenChange={setOpen}>
        <p>dialog body</p>
      </Dialog>
      <Selector label="Pick" value={sel} onChange={setSel} options={['a', 'b']} />
      <Switch label="Sw" value={sw} onChange={setSw} />
      <SegmentedControl label="Seg" value={seg} onChange={setSeg}>
        <SegmentedControlItem value="x" label="X" />
        <SegmentedControlItem value="y" label="Y" />
      </SegmentedControl>
      <Table data={[{ id: '1', n: 'one' }]} columns={[{ key: 'n', header: 'N' }]} idKey="id" />
      <span data-testid="out">{`${v}|${sel}|${sw}|${seg}`}</span>
    </div>
  );
}

test('astryx renders in jsdom', async () => {
  const u = userEvent.setup();
  render(
    <Theme theme={neutralTheme}>
      <LayerProvider>
      <Demo />
      </LayerProvider>
    </Theme>,
  );
  await u.type(screen.getByLabelText('Name'), 'abc');
  await u.click(screen.getByRole('switch', { name: 'Sw' }));
  await u.click(screen.getByRole('radio', { name: 'Y' }));
  await u.click(screen.getByRole('button', { name: 'Open' }));
  expect(await screen.findByText('dialog body')).toBeInTheDocument();
  expect(screen.getByText('one')).toBeInTheDocument();
  await u.click(screen.getByRole('button', { name: 'Toast' }));
  expect(await screen.findByText('hello toast')).toBeInTheDocument();
  expect(screen.getByTestId('out').textContent).toBe('abc|a|true|y');
});
