import { act, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderWithApp } from '../test/utils';
import { BlockingPanel, resumeAt } from './BlockingControl';

const T0 = Date.UTC(2026, 9, 10, 18, 0, 0);

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
    const sel = screen.getByRole('combobox', { name: 'Other duration' });
    fireEvent.change(sel, { target: { value: '2h' } });
    fireEvent.click(screen.getByRole('button', { name: 'Disable' }));
    expect(onSet).toHaveBeenLastCalledWith({ blocking: false, timer: 7200 });
    onSet.mockClear();
    fireEvent.click(screen.getByRole('button', { name: 'Disable blocking' }));
    fireEvent.change(screen.getByRole('combobox', { name: 'Other duration' }), { target: { value: '__custom__' } });
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
