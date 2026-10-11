import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import type { ClientMatcher } from '../../api/types';
import type { Schedule } from '../../lib/policies';
import { renderWithApp } from '../../test/utils';
import { findSelector, getSelector, optionLabels, pick } from '../../test/controls';
import { ServiceGrid } from '../ServiceGrid';
import { monogram, ServiceIcon, toneOf } from '../ServiceIcon';
import { cleanMatchers, matcherError, matcherFromText, matchersError, MatchersField } from './MatchersField';
import { ObjectRefPicker } from './ObjectRefPicker';
import { ScheduleField } from './ScheduleField';

function Stateful<T>({ initial, render: r }: { initial: T; render: (v: T, set: (v: T) => void) => React.ReactNode }) {
  const [v, set] = useState(initial);
  return <>{r(v, set)}</>;
}

describe('ServiceIcon', () => {
  it('shows the catalogue logo under the API base, or a stable monogram', () => {
    renderWithApp(
      <>
        <ServiceIcon id="youtube" name="YouTube" icon="/api/v1/catalogue/icons/youtube.svg" />
        <ServiceIcon id="roblox" name="Roblox" />
      </>,
    );
    expect(screen.getByTestId('icon-youtube')).toHaveAttribute('src', expect.stringMatching(/\/api\/v1\/catalogue\/icons\/youtube\.svg$/));
    expect(screen.getByTestId('monogram-roblox')).toHaveTextContent('R');
    fireEvent.error(screen.getByTestId('icon-youtube'));
    expect(screen.getByTestId('monogram-youtube')).toHaveTextContent('Y');
    expect([monogram('X (Twitter)'), monogram('9GAG'), monogram('Riot Games (League of Legends)'), monogram('Disney+')]).toEqual(['X', '9G', 'RG', 'D']);
    expect(toneOf('roblox')).toBe(toneOf('roblox'));
  });

  it('carries the dark-background variant next to the default logo', () => {
    renderWithApp(<ServiceIcon id="tiktok" name="TikTok" icon="/api/v1/catalogue/icons/tiktok.svg" iconOnDark="/api/v1/catalogue/icons/tiktok-light.svg" />);
    expect(screen.getByTestId('icon-tiktok')).toHaveClass('k-icon-on-light');
    expect(screen.getByTestId('icon-tiktok-dark')).toHaveAttribute('src', expect.stringMatching(/tiktok-light\.svg$/));
    expect(screen.getByTestId('icon-tiktok-dark')).toHaveClass('k-icon-on-dark');
  });
});

describe('ServiceGrid', () => {
  it('toggles services, filters and keeps IDs the catalogue lacks', async () => {
    const onChange = vi.fn();
    renderWithApp(<ServiceGrid label="Blocked services" value={['tiktok', 'myspace']} onChange={onChange} />);
    const tiktok = await screen.findByRole('button', { name: /TikTok/ });
    expect(tiktok).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: /myspace/ })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('Not in the catalogue (ignored until it is)')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /YouTube/ }));
    expect(onChange).toHaveBeenLastCalledWith(['tiktok', 'myspace', 'youtube']);
    fireEvent.click(tiktok);
    expect(onChange).toHaveBeenLastCalledWith(['myspace']);
    fireEvent.change(screen.getByRole('textbox', { name: 'Filter services' }), { target: { value: 'gaming' } });
    expect(screen.getByRole('button', { name: /Roblox/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /YouTube/ })).toBeNull();
  });
});

describe('ObjectRefPicker', () => {
  it('offers bare names of one namespace with sameNamespace and keeps missing refs', async () => {
    const onChange = vi.fn();
    renderWithApp(<ObjectRefPicker kind="clientgroups" label="Client groups" multiple sameNamespace value={['kids', 'gone']} onChange={onChange} />);
    await screen.findByRole('checkbox', { name: 'iot' });
    const kids = screen.getByRole('checkbox', { name: 'kids' });
    expect(kids).toBeChecked();
    expect(screen.getByRole('checkbox', { name: /gone/ })).toBeChecked();
    expect(screen.getAllByText('missing')[0]).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: 'iot' }));
    expect(onChange).toHaveBeenLastCalledWith(['kids', 'gone', 'iot']);
  });

  it('answers namespace/name refs without sameNamespace', async () => {
    const onChange = vi.fn();
    renderWithApp(<ObjectRefPicker kind="clientgroups" label="Group" value={[]} onChange={onChange} />);
    const select = await findSelector('Group');
    await waitFor(() => expect(optionLabels(select)).toContain('default/kids'));
    pick(select, /^default\/kids/);
    expect(onChange).toHaveBeenLastCalledWith(['default/kids']);
  });
});

describe('MatchersField', () => {
  it('validates each matcher for its kind', () => {
    expect(matcherError({ ip: '192.168.1.300' })).toBe('Not an IPv4 or IPv6 address');
    expect(matcherError({ cidr: '10.0.0.0' })).toMatch(/address\/prefix/);
    expect(matcherError({ cidr: '10.0.0.0/33' })).toBe('IPv4 prefix length is at most 32');
    expect(matcherError({ mac: 'aa:bb' })).toMatch(/MAC address/);
    expect(matcherError({ clientID: 'kids tablet' })).toMatch(/Letters, digits/);
    expect(matcherError({ ip: '' })).toBe('Required');
    expect(matcherError({ clientID: 'kids-tablet' })).toBeNull();
    expect(matchersError([{ ip: '10.0.0.1' }, { ip: 'x' }, { mac: 'y' }])).toBe('Matcher 2: Not an IPv4 or IPv6 address (and 1 more)');
    expect(['10.0.0.0/8', 'AA:BB:CC:DD:EE:FF', '10.0.0.7', 'tablet'].map(matcherFromText)).toEqual([{ cidr: '10.0.0.0/8' }, { mac: 'aa:bb:cc:dd:ee:ff' }, { ip: '10.0.0.7' }, { clientID: 'tablet' }]);
    expect(cleanMatchers([{ ip: ' 10.0.0.1 ', name: ' ', comment: 'c' }])).toEqual([{ ip: '10.0.0.1', comment: 'c' }]);
  });

  it('switches kinds, keeps one selector per matcher and pastes many', () => {
    let last: ClientMatcher[] = [];
    render(
      <Stateful<ClientMatcher[]>
        initial={[{ ip: '192.168.1.30', name: 'tablet' }]}
        render={(v, set) => (
          <MatchersField
            value={v}
            onChange={(n) => {
              last = n;
              set(n);
            }}
          />
        )}
      />,
    );
    pick(getSelector('Matcher 1 kind'), 'CIDR range');
    expect(last).toEqual([{ cidr: '192.168.1.30', name: 'tablet' }]);
    expect(screen.getByText(/An address\/prefix such as/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 1 CIDR range' }), { target: { value: '192.168.1.0/24' } });
    expect(screen.queryByText(/An address\/prefix such as/)).toBeNull();
    pick(getSelector('Matcher 1 kind'), 'MAC address');
    expect(screen.getByText(/inventory integrations arrive in v0.2/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Paste many' }));
    fireEvent.change(screen.getByRole('textbox', { name: /Paste addresses/ }), { target: { value: '10.0.0.1, 10.1.0.0/16\nphone' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add 3 matchers' }));
    expect(last.slice(1)).toEqual([{ ip: '10.0.0.1' }, { cidr: '10.1.0.0/16' }, { clientID: 'phone' }]);
    fireEvent.click(screen.getAllByRole('button', { name: 'Remove' })[0]!);
    expect(last).toHaveLength(3);
  });
});

describe('ScheduleField', () => {
  it('switches between always and a window and toggles weekdays in API order', () => {
    let last: Schedule | undefined;
    render(
      <Stateful<Schedule | undefined>
        initial={undefined}
        render={(v, set) => (
          <ScheduleField
            value={v}
            now={new Date('2026-10-12T17:00:00Z')}
            onChange={(n) => {
              last = n;
              set(n);
            }}
          />
        )}
      />,
    );
    expect(screen.getByText('The policy applies at all times.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('radio', { name: 'Only during a time window' }));
    expect(last).toMatchObject({ weekdays: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu'], start: '21:00', end: '07:00' });
    pick(getSelector('Time zone'), 'America/Chicago');
    fireEvent.click(screen.getByRole('button', { name: 'Sun' }));
    fireEvent.click(screen.getByRole('button', { name: 'Sat' }));
    expect(last?.weekdays).toEqual(['Mon', 'Tue', 'Wed', 'Thu', 'Sat']);
    expect(screen.getByRole('button', { name: 'Sat' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByTestId('schedule-summary')).toHaveTextContent('Mon Tue Wed Thu Sat 21:00–07:00 (America/Chicago) · off right now');
    fireEvent.click(screen.getByRole('radio', { name: 'Always' }));
    expect(last).toBeUndefined();
  });
});
