import { fireEvent, render, screen, within } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import type { UiShape } from '../../lib/settingsMeta';
import { fromDraft, shapeError, toDraft } from '../../lib/shapes';
import { pick, typeNumber } from '../../test/controls';
import { ShapeField } from './ShapeField';

// Representative nested metadata from internal/config/schema.go. These cover
// the list, string-list, object, map-key and map-value paths of the real schema.
const listenerShape: UiShape = {
  type: 'list',
  help: 'Addresses where devices send plain DNS queries.',
  items: {
    type: 'object',
    help: 'One listening address and the DNS transports it serves.',
    fields: [
      { name: 'address', label: 'Address', type: 'string', format: 'hostport', help: 'Address and port to listen on.' },
      {
        name: 'protocols', label: 'Protocols', type: 'string[]',
        help: 'Transports served on this address: udp, tcp or both.',
        items: {
          type: 'string', help: 'Choose one transport for this listener.',
          enum: [
            { value: 'udp', label: 'UDP', help: 'Plain DNS over UDP; what most devices use.' },
            { value: 'tcp', label: 'TCP', help: 'Plain DNS over TCP; used for large answers and by clients that retry over TCP.' },
          ],
        },
      },
    ],
  },
};

function before(control: HTMLElement, help: HTMLElement): boolean {
  return !!(control.compareDocumentPosition(help) & Node.DOCUMENT_POSITION_FOLLOWING);
}

describe('nested Shape help', () => {
  it('describes list and object groups, preserving field and selected option help', () => {
    function Listeners() {
      const [value, set] = useState([{ address: '0.0.0.0:53', protocols: ['udp'] }]);
      return <ShapeField label="DNS listeners" shape={listenerShape} value={value} onChange={(v) => set(v as typeof value)} />;
    }
    render(<Listeners />);
    expect(screen.getByRole('group', { name: 'DNS listeners' })).toHaveAccessibleDescription(listenerShape.help);
    expect(screen.getByRole('group', { name: 'DNS listeners 1' })).toHaveAccessibleDescription(listenerShape.items?.help);
    expect(screen.getByRole('textbox', { name: 'Address' })).toHaveAccessibleDescription('Address and port to listen on.');
    expect(screen.getByRole('group', { name: 'Protocols' })).toHaveAccessibleDescription('Transports served on this address: udp, tcp or both.');
    const protocol = screen.getByRole('combobox', { name: 'Protocols 1' });
    expect(protocol).toHaveAccessibleDescription('Choose one transport for this listener. Plain DNS over UDP; what most devices use.');
    expect(before(protocol, screen.getByText(/Choose one transport/))).toBe(true);
    pick(protocol, 'TCP');
    expect(protocol).toHaveAccessibleDescription('Choose one transport for this listener. Plain DNS over TCP; used for large answers and by clients that retry over TCP.');
    expect(screen.queryByText('Plain DNS over UDP; what most devices use.')).not.toBeInTheDocument();
  });

  it('recurses from a named upstream map into object and string-list values without changing edits', () => {
    const shape: UiShape = {
      type: 'map', keyLabel: 'Group',
      keys: { type: 'string', help: 'Group name used by policies and conditional forwarding; keep a group named default.' },
      values: {
        type: 'object', help: 'How this named group resolves queries.',
        fields: [{
          name: 'servers', label: 'Servers', type: 'string[]', help: 'DNS servers this group forwards to.',
          items: { type: 'string', format: 'upstream', help: 'One server address or URL; prefix name= to give it a display name.' },
        }],
      },
    };
    const initial = { default: { servers: ['1.1.1.1:53'], extra: true } };
    const onChange = vi.fn();
    render(<ShapeField label="Upstream groups" shape={shape} value={toDraft(shape, initial)} onChange={onChange} />);
    expect(screen.getByRole('textbox', { name: 'Group' })).toHaveAccessibleDescription(shape.keys?.help);
    expect(screen.getByRole('group', { name: 'default' })).toHaveAccessibleDescription(shape.values?.help);
    expect(screen.getByRole('group', { name: 'Servers' })).toHaveAccessibleDescription('DNS servers this group forwards to.');
    const server = screen.getByRole('textbox', { name: 'Servers 1' });
    expect(server).toHaveAccessibleDescription('One server address or URL; prefix name= to give it a display name.');
    expect(before(server, screen.getByText(/One server address/))).toBe(true);
    fireEvent.change(server, { target: { value: 'tls://dns.quad9.net' } });
    const draft = onChange.mock.calls[0]?.[0];
    expect(fromDraft(shape, draft)).toEqual({ default: { servers: ['tls://dns.quad9.net'], extra: true } });
    expect(shapeError(shape, draft)).toBeNull();
  });

  it('links numeric map keys and scalar values to their help on every row', () => {
    const shape: UiShape = {
      type: 'map', keyLabel: 'Prefix length',
      help: 'Queries per minute allowed per IPv4 address or network, by prefix length.',
      keys: { type: 'int', min: 0, max: 32, help: 'IPv4 network prefix length: 32 limits one address; 24 limits a /24 network.' },
      values: { type: 'int', min: 0, help: 'Queries per minute for this prefix length; 0 disables this limit.' },
    };
    const onChange = vi.fn();
    render(<ShapeField label="IPv4 limits" shape={shape} value={toDraft(shape, { '24': 6000, '32': 600 })} onChange={onChange} />);
    expect(screen.getByRole('group', { name: 'IPv4 limits' })).toHaveAccessibleDescription(shape.help);
    const rows = screen.getAllByTestId('map-row');
    for (const row of rows) {
      const key = within(row).getByRole('spinbutton', { name: 'Prefix length' });
      const value = within(row).getByRole('spinbutton', { name: 'Value' });
      expect(key).toHaveAccessibleDescription(/IPv4 network prefix length: 32 limits one address; 24 limits a \/24 network\./);
      expect(value).toHaveAccessibleDescription(/Queries per minute for this prefix length; 0 disables this limit\./);
      expect(before(key, within(row).getByText(/IPv4 network prefix length/))).toBe(true);
      expect(before(value, within(row).getByText(/Queries per minute for this prefix length/))).toBe(true);
    }
    // Repeated help must describe the row's own control, with distinct IDs.
    const keyHelp = rows.map((row) => within(row).getByText(/IPv4 network prefix length/).id);
    expect(new Set(keyHelp).size).toBe(2);
    typeNumber(within(rows[0]!).getByRole('spinbutton', { name: 'Value' }), '9000');
    expect(fromDraft(shape, onChange.mock.calls.at(-1)?.[0])).toEqual({ '24': 9000, '32': 600 });
  });

  it('shows text map key and scalar value help and preserves key edits', () => {
    const shape: UiShape = {
      type: 'map', keyLabel: 'Domain suffix',
      keys: { type: 'string', help: 'Domain suffix to route, such as corp.example, or a dnsmasq suffix list like [/corp.example/10.in-addr.arpa/].' },
      values: { type: 'string', help: 'Upstream group that receives queries for this suffix.' },
    };
    const onChange = vi.fn();
    render(<ShapeField label="Conditional forwarding" shape={shape} value={toDraft(shape, { 'corp.example': 'default' })} onChange={onChange} />);
    const key = screen.getByRole('textbox', { name: 'Domain suffix' });
    expect(key).toHaveAccessibleDescription(shape.keys?.help);
    expect(screen.getByRole('textbox', { name: 'Value' })).toHaveAccessibleDescription(shape.values?.help);
    fireEvent.change(key, { target: { value: 'lab.example' } });
    expect(fromDraft(shape, onChange.mock.calls[0]?.[0])).toEqual({ 'lab.example': 'default' });
  });

  it('keeps explicit field help precedence and accepts schemas without nested help', () => {
    const { rerender } = render(<ShapeField label="Name" description="Field help." shape={{ type: 'string', help: 'Shape help.' }} value="default" onChange={() => {}} />);
    expect(screen.getByRole('textbox', { name: 'Name' })).toHaveAccessibleDescription('Field help.');
    expect(screen.queryByText('Shape help.')).not.toBeInTheDocument();
    rerender(<ShapeField label="Name" shape={{ type: 'string' }} value="default" onChange={() => {}} />);
    expect(screen.getByRole('textbox', { name: 'Name' })).not.toHaveAttribute('aria-describedby');
    rerender(<ShapeField label="Servers" shape={{ type: 'string[]', items: { type: 'string' } }} value={['1.1.1.1:53']} onChange={() => {}} />);
    expect(screen.getByRole('group', { name: 'Servers' })).not.toHaveAttribute('aria-describedby');
    expect(screen.getByRole('textbox', { name: 'Servers 1' })).not.toHaveAttribute('aria-describedby');
  });
});
