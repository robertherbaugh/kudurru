import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { getSelector, pick } from '../test/controls';
import { API, renderWithApp } from '../test/utils';
import { server } from '../test/setup';
import { qk } from '../api/context';
import { BlockingControl, BlockingPanel, resumeAt } from './BlockingControl';

const T0 = Date.UTC(2026, 9, 10, 18, 0, 0);

it('keeps observed local state after a forwarded commit and refetches without replay', async () => {
  let reads = 0;
  let writes = 0;
  server.use(
    http.get(`${API}/dns/blocking`, () => { reads++; return HttpResponse.json({ blocking: true, configured: true, scope: 'cluster', node: 'receiver' }); }),
    http.post(`${API}/dns/blocking`, () => {
      writes++;
      return HttpResponse.json({ committed: true, settingsUID: 'settings', resourceVersion: 'v2', disabled: true, publication: 'pending' }, { status: 202 });
    }),
  );
  const { client } = renderWithApp(<BlockingControl />);
  fireEvent.click(await screen.findByRole('button', { name: 'Disable blocking' }));
  fireEvent.click(screen.getByRole('button', { name: '30 seconds' }));
  expect(await screen.findByText('Change saved; waiting for this node to apply it')).toBeInTheDocument();
  await waitFor(() => expect(reads).toBeGreaterThanOrEqual(2));
  expect(writes).toBe(1);
  expect(client.getQueryData(qk.blocking)).toMatchObject({ blocking: true, node: 'receiver' });
  expect(screen.getByText('Blocking enabled')).toBeInTheDocument();
});

describe('resumeAt', () => {
  it('prefers until, falls back to timer from the fetch time, and is empty while blocking', () => {
    expect(resumeAt({ blocking: false, until: '2026-10-10T18:05:00Z' }, T0)).toBe('2026-10-10T18:05:00Z');
    expect(resumeAt({ blocking: false, timer: 30 }, T0)).toBe('2026-10-10T18:00:30.000Z');
    expect(resumeAt({ blocking: false }, T0)).toBeUndefined();
    expect(resumeAt({ blocking: true, timer: 30 }, T0)).toBeUndefined();
  });
});

describe('BlockingPanel', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('counts down to re-enabling and reports the expiry once', () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval', 'setTimeout', 'clearTimeout', 'Date'] });
    vi.setSystemTime(T0);
    const onExpire = vi.fn();
    renderWithApp(
      <BlockingPanel state={{ blocking: false, until: new Date(T0 + 30_000).toISOString() }} fetchedAt={T0} canToggle onSet={vi.fn()} onExpire={onExpire} />,
      { user: null },
    );
    expect(screen.getByText('Blocking disabled, re-enables in 0:30')).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(5_000);
    });
    expect(screen.getByText('Blocking disabled, re-enables in 0:25')).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(25_000);
    });
    expect(screen.getByText('Blocking disabled, re-enables in 0:00')).toBeInTheDocument();
    expect(onExpire).toHaveBeenCalledTimes(1);
    act(() => {
      vi.advanceTimersByTime(5_000);
    });
    expect(onExpire).toHaveBeenCalledTimes(1);
  });

  it('shows indefinite disabling and enables with one click', () => {
    const onSet = vi.fn();
    renderWithApp(<BlockingPanel state={{ blocking: false }} fetchedAt={T0} canToggle onSet={onSet} />, { user: null });
    expect(screen.getByText('Blocking disabled indefinitely')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Enable blocking' }));
    expect(onSet).toHaveBeenCalledWith({ blocking: true });
  });

  it.each([
    ['10 seconds', { blocking: false, timer: 10 }],
    ['30 seconds', { blocking: false, timer: 30 }],
    ['5 minutes', { blocking: false, timer: 300 }],
    ['Indefinitely', { blocking: false }],
  ])('disables for the "%s" preset', (label, want) => {
    const onSet = vi.fn();
    renderWithApp(<BlockingPanel state={{ blocking: true }} fetchedAt={T0} canToggle onSet={onSet} />, { user: null });
    expect(screen.getByText('Blocking enabled')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Disable blocking' }));
    fireEvent.click(screen.getByRole('button', { name: label }));
    expect(onSet).toHaveBeenCalledWith(want);
  });

  it('disables for a preset duration, a custom one in minutes or as a Go duration, and rejects garbage', () => {
    const onSet = vi.fn();
    renderWithApp(<BlockingPanel state={{ blocking: true }} fetchedAt={T0} canToggle onSet={onSet} />, { user: null });
    fireEvent.click(screen.getByRole('button', { name: 'Disable blocking' }));
    pick(getSelector('Other duration'), '2 hours');
    fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
    expect(onSet).toHaveBeenLastCalledWith({ blocking: false, timer: 7200 });
    onSet.mockClear();
    fireEvent.click(screen.getByRole('button', { name: 'Disable blocking' }));
    pick(getSelector('Other duration'), 'Custom…');
    const input = screen.getByRole('textbox', { name: 'Other duration (custom)' });
    fireEvent.change(input, { target: { value: 'soon' } });
    fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
    expect(onSet).not.toHaveBeenCalled();
    expect(screen.getByText(/Minutes \(e\.g\. 15\) or a duration/)).toBeInTheDocument();
    fireEvent.change(input, { target: { value: '1h30m' } });
    fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
    expect(onSet).toHaveBeenLastCalledWith({ blocking: false, timer: 5400 });
  });

  it('disables the toggle without the permission', () => {
    renderWithApp(<BlockingPanel state={{ blocking: true }} fetchedAt={T0} canToggle={false} onSet={vi.fn()} />, { user: null });
    expect(screen.getByRole('button', { name: 'Disable blocking' })).toHaveAttribute('aria-disabled', 'true');
  });
});
