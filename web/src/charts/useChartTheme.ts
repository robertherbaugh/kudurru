// Charts paint on canvas, so they must redraw when the colours behind the
// CSS custom properties change: a different theme, mode, contrast, or the
// OS switching between light and dark while the mode is "auto".
import { useEffect, useState } from 'react';
import { useAppearance } from '../lib/theme';

/** A key that changes whenever chart colours must be re-read from CSS. */
export function useChartTheme(): string {
  const { mode, theme, contrast } = useAppearance();
  const [dark, setDark] = useState(() => prefersDark());
  useEffect(() => {
    const mq = window.matchMedia?.('(prefers-color-scheme: dark)');
    if (!mq) return undefined;
    const on = () => setDark(mq.matches);
    mq.addEventListener?.('change', on);
    return () => mq.removeEventListener?.('change', on);
  }, []);
  // The Theme component applies the new tokens in an effect; read the
  // colours on the next frame so they are the new ones.
  const [settled, setSettled] = useState(0);
  useEffect(() => {
    const id = window.requestAnimationFrame(() => setSettled((n) => n + 1));
    return () => window.cancelAnimationFrame(id);
  }, [mode, theme, contrast, dark]);
  return `${mode}:${theme}:${contrast}:${mode === 'system' && dark ? 'dark' : 'light'}:${settled}`;
}

function prefersDark(): boolean {
  try {
    return window.matchMedia('(prefers-color-scheme: dark)').matches;
  } catch {
    return false;
  }
}
