import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { fromDraft, shapeError, toDraft } from '../../lib/shapes';
import type { UiShape } from '../../lib/settingsMeta';
import { BytesField, joinBytes, splitBytes } from './BytesField';
import { DurationField, durationLabel, durationPresets } from './DurationField';
import { EnumSelect } from './EnumSelect';
import { formatError } from './formats';
import { NumberField, numberError } from './NumberField';
import { SecondsField, secondsError } from './SecondsField';
import { StringListField, listError } from './StringListField';
import { SelectField } from './SelectField';
import { CheckboxField, SwitchField, TextAreaField, TextField } from './TextField';
import { getSelector, optionLabels, optionsOf, pick, typeNumber, valueOf } from '../../test/controls';

function Controlled<T>({ initial, children }: { initial: T; children: (v: T, set: (v: T) => void) => React.ReactNode }) {
  const [v, set] = useState(initial);
  return <>{children(v, set)}</>;
}

describe('formats', () => {
  it.each([
    ['ip', '192.0.2.1', null],
    ['ip', '2001:db8::1', null],
    ['ip', 'example.com', 'Not an IP address'],
    ['ipv4', '2001:db8::1', 'Not an IPv4 address'],
    ['cidr', '10.0.0.0/8', null],
    ['cidr', '10.0.0.1', null],
    ['cidr', '10.0.0.0/33', 'The prefix length must be 0-32'],
    ['hostport', ':80', null],
    ['hostport', '0.0.0.0:99999', 'The port must be 1-65535'],
    ['url', 'https://example.com/a', null],
    ['url', 'ftp://x', 'An http:// or https:// URL'],
    ['domain', 'bad..name', 'Empty label (two dots in a row)'],
    ['upstream', 'tls://dns.quad9.net', null],
    ['email', 'admin@example.com', null],
    ['email', 'admin', 'Not an e-mail address'],
  ] as const)('%s %s', (f, v, want) => expect(formatError(f, v)).toBe(want));
});

describe('field helpers', () => {
  it('labels and checks durations, seconds, numbers and sizes', () => {
    expect(durationLabel('72h')).toBe('3 days');
    expect(durationLabel('1800ms')).toBe('1800 ms');
    expect(durationPresets(['0s', '10m']).map((p) => p.label)).toEqual(['0 (off)', '10 minutes']);
    expect(secondsError('', { optional: true })).toBeNull();
    expect(secondsError('-1')).toBe('Seconds, 0 to 2147483647');
    expect(numberError('5', { min: 0, max: 4 })).toBe('Between 0 and 4');
    expect(numberError('1.5')).toBe('Must be a whole number');
    expect(splitBytes('10GiB')).toEqual({ n: '10', unit: 'GiB' });
    expect(splitBytes(2048)).toEqual({ n: '2048', unit: 'B' });
    expect(joinBytes('500', 'MB')).toBe('500MB');
    expect(joinBytes('42', 'B')).toBe(42);
    expect(listError(['1.1.1.1', 'nope/x'], (s) => formatError('ip', s))).toBe('Line 2 (nope/x): Not an IP address');
  });

  it('round-trips map, list and object drafts and finds the first error', () => {
    const shape: UiShape = {
      type: 'map',
      keyLabel: 'Group',
      values: {
        type: 'object',
        fields: [
          { name: 'servers', type: 'string[]', items: { type: 'string', format: 'upstream' } },
          { name: 'timeout', type: 'duration' },
          { name: 'weight', type: 'int', min: 0, max: 10 },
        ],
      },
    };
    const v = { default: { servers: ['1.1.1.1'], timeout: '2s', extra: true } };
    const d = toDraft(shape, v);
    expect(fromDraft(shape, d)).toEqual(v);
    expect(shapeError(shape, d)).toBeNull();
    const bad = { __rows: [['default', { servers: ['ftp://x'], timeout: '2s' }], ['default', {}]] };
    expect(shapeError(shape, bad)).toMatch(/^default servers: ftp:\/\/x: Unknown scheme/);
    expect(shapeError(shape, { __rows: [['a', {}], ['a', {}]] })).toBe('a is listed twice');
    expect(shapeError(shape, { __rows: [['a', { weight: '11' }]] })).toBe('a weight: Between 0 and 10');
  });
});

describe('typed fields', () => {
  it('EnumSelect shows the chosen option help and keeps an unknown value', () => {
    const onChange = vi.fn();
    render(<EnumSelect label="Mode" value="other" options={[{ value: 'a', label: 'A', help: 'Help A' }]} onChange={onChange} />);
    const sel = screen.getByRole('combobox', { name: 'Mode' });
    expect(optionLabels(sel)).toEqual(['A', 'other']);
    pick(sel, 'A');
    expect(onChange).toHaveBeenCalledWith('a');
  });

  it('DurationField matches "10m0s" to its preset and offers a custom value', () => {
    render(<Controlled initial="10m0s">{(v, set) => <DurationField label="TTL" value={v} onChange={set} />}</Controlled>);
    const sel = screen.getByRole('combobox', { name: 'TTL' });
    expect(valueOf(sel)).toBe('10m');
    pick(sel, 'Custom…');
    fireEvent.change(screen.getByRole('textbox', { name: 'TTL (custom)' }), { target: { value: '45m' } });
    expect(screen.getByRole('textbox', { name: 'TTL (custom)' })).toHaveValue('45m');
  });

  it('DurationField with emptyLabel keeps "" as its own choice (e.g. Permanently)', () => {
    render(<Controlled initial="">{(v, set) => <DurationField label="Duration" value={v} onChange={set} emptyLabel="Permanently" presets={[{ value: '1h', label: '1 hour' }]} />}</Controlled>);
    const sel = screen.getByRole('combobox', { name: 'Duration' });
    expect(valueOf(sel)).toBe('');
    expect(sel).toHaveTextContent('Permanently');
    expect(screen.queryByRole('textbox', { name: 'Duration (custom)' })).toBeNull();
    pick(sel, '1 hour');
    expect(valueOf(sel)).toBe('1h');
    pick(sel, 'Permanently');
    expect(valueOf(sel)).toBe('');
  });

  it('EnumSelect lists a disabled option without letting it be chosen', () => {
    render(<EnumSelect label="Scope" value="a" options={[{ value: 'a', label: 'A' }, { value: 'b', label: 'B', disabled: true }]} onChange={() => {}} />);
    const [a, b] = optionsOf(screen.getByRole('combobox', { name: 'Scope' }));
    expect(b).toHaveAttribute('aria-disabled', 'true');
    expect(a).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('SecondsField picks TTL presets and shows them in words', () => {
    const onChange = vi.fn();
    render(<SecondsField label="TTL" value="" emptyLabel="Zone default" onChange={onChange} />);
    const sel = screen.getByRole('combobox', { name: 'TTL' });
    expect(optionLabels(sel)).toContain('5 minutes (300)');
    pick(sel, '1 hour (3600)');
    expect(onChange).toHaveBeenCalledWith('3600');
  });

  it('NumberField reports values outside its bounds', () => {
    render(<NumberField label="Prefix" value="40" onChange={() => {}} min={0} max={32} />);
    expect(screen.getByRole('spinbutton', { name: 'Prefix' })).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText('Between 0 and 32')).toBeInTheDocument();
    expect(screen.getByText('Range 0–32.')).toBeInTheDocument();
  });

  it('NumberField commits typed text as a number string', () => {
    const onChange = vi.fn();
    render(<NumberField label="Prefix" value="" onChange={onChange} min={0} max={32} />);
    typeNumber(screen.getByRole('spinbutton', { name: 'Prefix' }), '24');
    expect(onChange).toHaveBeenLastCalledWith('24');
  });

  it('BytesField keeps the unit when the number changes', () => {
    const onChange = vi.fn();
    render(<BytesField label="Size" value="10GiB" onChange={onChange} />);
    typeNumber(screen.getByRole('spinbutton', { name: 'Size' }), '20');
    expect(onChange).toHaveBeenCalledWith('20GiB');
    pick(screen.getByRole('combobox', { name: 'Size unit' }), 'MB');
    expect(onChange).toHaveBeenCalledWith('10MB');
  });

  it('StringListField adds, edits, pastes and removes rows', () => {
    render(
      <Controlled initial={['a.example']}>
        {(v, set) => (
          <>
            <StringListField label="Names" value={v} onChange={set} itemError={(s) => formatError('domain', s)} />
            <output data-testid="out">{JSON.stringify(v)}</output>
          </>
        )}
      </Controlled>,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Add' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Names 2' }), { target: { value: 'bad..x' } });
    expect(screen.getAllByText('Empty label (two dots in a row)').length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole('button', { name: 'Remove Names 2' }));
    fireEvent.click(screen.getByRole('button', { name: 'Paste many' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Paste entries, one per line' }), { target: { value: 'b.example\nc.example\n' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add these' }));
    expect(screen.getByTestId('out')).toHaveTextContent('["a.example","b.example","c.example"]');
  });
});

describe('field wrappers: help under the control', () => {
  /** Whether a comes before b in the document. */
  const before = (a: Element, b: Element) => Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);
  const describedBy = (el: Element) => (el.getAttribute('aria-describedby') ?? '').split(' ').filter(Boolean);

  it('SelectField renders the help after the control and links it', () => {
    render(<SelectField label="Mode" help="How it answers." value="a" options={[{ value: 'a', label: 'A' }]} onChange={() => {}} />);
    const sel = screen.getByRole('combobox', { name: 'Mode' });
    const help = screen.getByText('How it answers.');
    expect(before(sel, help)).toBe(true);
    expect(before(screen.getByText('Mode'), sel)).toBe(true);
    expect(describedBy(sel)).toContain(help.id);
    expect(sel).toHaveAccessibleDescription('How it answers.');
  });

  it('SelectField shows the selected option help, then the error after it', () => {
    render(
      <Controlled initial="a">
        {(v, set) => (
          <SelectField
            label="Mode"
            value={v}
            options={[
              { value: 'a', label: 'A', help: 'Help of A.' },
              { value: 'b', label: 'B', help: 'Help of B.' },
            ]}
            onChange={set}
            error={v === 'b' ? 'B is not allowed here' : undefined}
          />
        )}
      </Controlled>,
    );
    const sel = screen.getByRole('combobox', { name: 'Mode' });
    expect(sel).toHaveAccessibleDescription('Help of A.');
    pick(sel, 'B');
    expect(screen.queryByText('Help of A.')).toBeNull();
    const help = screen.getByText('Help of B.');
    const err = screen.getByText('B is not allowed here');
    expect(before(help, err)).toBe(true);
    expect(sel).toHaveAttribute('aria-invalid', 'true');
    expect(describedBy(sel)).toEqual(expect.arrayContaining([help.id, err.closest('[id]')?.id]));
  });

  it('SelectField offers an empty choice and a search for long lists', () => {
    const options = Array.from({ length: 20 }, (_, i) => ({ value: `v${i}`, label: `Value ${i}` }));
    const onChange = vi.fn();
    render(<SelectField label="Many" value="" emptyLabel="Not set" options={options} onChange={onChange} />);
    const sel = getSelector('Many');
    expect(sel).toHaveTextContent('Not set');
    expect(optionLabels(sel)[0]).toBe('Not set');
    pick(sel, 'Value 7');
    expect(onChange).toHaveBeenCalledWith('v7');
  });

  it('TextField, TextAreaField and NumberField put help and error under the control, linked', () => {
    render(
      <>
        <TextField label="Name" help="A DNS name." error="Required" value="" onChange={() => {}} />
        <TextAreaField label="Notes" help="Free text." value="" onChange={() => {}} />
        <NumberField label="Port" help="The UDP port." value="53" onChange={() => {}} min={1} max={65535} unit="port" />
      </>,
    );
    const name = screen.getByRole('textbox', { name: 'Name' });
    expect(before(name, screen.getByText('A DNS name.'))).toBe(true);
    expect(before(screen.getByText('A DNS name.'), screen.getByText('Required'))).toBe(true);
    expect(name).toHaveAttribute('aria-invalid', 'true');
    expect(name).toHaveAccessibleDescription(/A DNS name\..*Required/);
    expect(screen.getByRole('textbox', { name: 'Notes' })).toHaveAccessibleDescription('Free text.');
    const port = screen.getByRole('spinbutton', { name: 'Port' });
    expect(port).toHaveAccessibleDescription(/The UDP port\. Range 1–65535\./);
    expect(before(port, screen.getByText(/The UDP port/))).toBe(true);
  });

  it('keeps the link when the control rewrites aria-describedby', async () => {
    render(<Controlled initial="">{(v, set) => <TextField label="Name" help="Help." error={v ? undefined : 'Required'} value={v} onChange={set} />}</Controlled>);
    const name = screen.getByRole('textbox', { name: 'Name' });
    fireEvent.change(name, { target: { value: 'x' } });
    expect(screen.queryByText('Required')).toBeNull();
    await waitFor(() => expect(name).toHaveAccessibleDescription('Help.'));
  });

  it('SwitchField and CheckboxField keep the help beside the control and add the error', () => {
    render(
      <>
        <SwitchField label="Logging" help="Writes every query." value onChange={() => {}} error="Not permitted" />
        <CheckboxField label="Agree" help="Required to continue." value={false} onChange={() => {}} />
      </>,
    );
    expect(screen.getByRole('switch', { name: 'Logging' })).toHaveAccessibleDescription(/Writes every query\..*Not permitted/);
    expect(screen.getByRole('checkbox', { name: 'Agree' })).toHaveAccessibleDescription('Required to continue.');
  });
});
