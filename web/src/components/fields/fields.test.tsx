import { fireEvent, render, screen } from '@testing-library/react';
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
    expect(Array.from((sel as HTMLSelectElement).options).map((o) => o.value)).toEqual(['a', 'other']);
    fireEvent.change(sel, { target: { value: 'a' } });
    expect(onChange).toHaveBeenCalledWith('a');
  });

  it('DurationField matches "10m0s" to its preset and offers a custom value', () => {
    render(<Controlled initial="10m0s">{(v, set) => <DurationField label="TTL" value={v} onChange={set} />}</Controlled>);
    const sel = screen.getByRole('combobox', { name: 'TTL' });
    expect(sel).toHaveValue('10m');
    fireEvent.change(sel, { target: { value: '__custom__' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'TTL (custom)' }), { target: { value: '45m' } });
    expect(screen.getByRole('textbox', { name: 'TTL (custom)' })).toHaveValue('45m');
  });

  it('SecondsField picks TTL presets and shows them in words', () => {
    const onChange = vi.fn();
    render(<SecondsField label="TTL" value="" emptyLabel="Zone default" onChange={onChange} />);
    const sel = screen.getByRole('combobox', { name: 'TTL' });
    expect(screen.getByRole('option', { name: '5 minutes (300)' })).toBeInTheDocument();
    fireEvent.change(sel, { target: { value: '3600' } });
    expect(onChange).toHaveBeenCalledWith('3600');
  });

  it('NumberField reports values outside its bounds', () => {
    render(<NumberField label="Prefix" value="40" onChange={() => {}} min={0} max={32} />);
    expect(screen.getByRole('spinbutton', { name: 'Prefix' })).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText('Between 0 and 32')).toBeInTheDocument();
  });

  it('BytesField keeps the unit when the number changes', () => {
    const onChange = vi.fn();
    render(<BytesField label="Size" value="10GiB" onChange={onChange} />);
    fireEvent.change(screen.getByRole('spinbutton'), { target: { value: '20' } });
    expect(onChange).toHaveBeenCalledWith('20GiB');
    fireEvent.change(screen.getByRole('combobox', { name: 'Size unit' }), { target: { value: 'MB' } });
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
