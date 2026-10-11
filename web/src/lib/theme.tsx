// Appearance: Astryx themes delivered as prebuilt CSS (D69) with
// auto/light/dark modes, a high-contrast overlay and one fun theme.
// The default, Kudurru, is the Neutral theme with the brand palette of
// docs/brand laid over it (styles.css, keyed on <html data-k-theme>):
// Paper and Ink surfaces, the Clay accent. Neutral ships in the initial
// bundle; Stone and Y2K are separate chunks loaded on first use. No theme
// injects <style> at runtime (built themes only), so the strict CSP holds.
import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import type { DefinedTheme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';

export type Mode = 'system' | 'light' | 'dark';
export type ThemeName = 'kudurru' | 'neutral' | 'stone' | 'y2k';
export type Contrast = 'normal' | 'more';

export const THEMES: { id: ThemeName; label: string }[] = [
  { id: 'kudurru', label: 'Kudurru' },
  { id: 'neutral', label: 'Neutral' },
  { id: 'stone', label: 'Stone' },
  { id: 'y2k', label: 'Y2K (fun)' },
];

interface Appearance {
  mode: Mode;
  theme: ThemeName;
  contrast: Contrast;
  setMode: (m: Mode) => void;
  setTheme: (t: ThemeName) => void;
  setContrast: (c: Contrast) => void;
}

const AppearanceContext = createContext<Appearance | null>(null);

const STORAGE_KEY = 'kudurru.appearance';
// Version 2 added the Kudurru theme. Version 1 stored "neutral" for
// everyone who never picked a theme, so it reads as the new default.
const STORAGE_VERSION = 2;

function load(): { mode: Mode; theme: ThemeName; contrast: Contrast } {
  const fallback = { mode: 'system' as Mode, theme: 'kudurru' as ThemeName, contrast: 'normal' as Contrast };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<typeof fallback> & { v?: number };
    const picked = v.theme === 'stone' || v.theme === 'y2k' || (v.theme === 'neutral' && v.v === STORAGE_VERSION);
    return {
      mode: v.mode === 'light' || v.mode === 'dark' ? v.mode : 'system',
      theme: picked ? (v.theme as ThemeName) : 'kudurru',
      contrast: v.contrast === 'more' ? 'more' : 'normal',
    };
  } catch {
    return fallback;
  }
}

async function loadTheme(name: ThemeName): Promise<DefinedTheme> {
  switch (name) {
    case 'stone': {
      const [mod] = await Promise.all([import('@astryxdesign/theme-stone/built'), import('@astryxdesign/theme-stone/theme.css')]);
      return mod.stoneTheme;
    }
    case 'y2k': {
      const [mod] = await Promise.all([import('@astryxdesign/theme-y2k/built'), import('@astryxdesign/theme-y2k/theme.css')]);
      return mod.y2kTheme;
    }
    default:
      return neutralTheme;
  }
}

export function AppearanceProvider({ children }: { children: ReactNode }) {
  const initial = useMemo(load, []);
  const [mode, setMode] = useState<Mode>(initial.mode);
  const [theme, setThemeName] = useState<ThemeName>(initial.theme);
  const [contrast, setContrast] = useState<Contrast>(initial.contrast);
  const [defined, setDefined] = useState<DefinedTheme>(neutralTheme);

  useEffect(() => {
    let live = true;
    loadTheme(theme)
      .then((t) => {
        if (live) setDefined(t);
      })
      .catch(() => {
        if (live) setDefined(neutralTheme);
      });
    return () => {
      live = false;
    };
  }, [theme]);

  useEffect(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ v: STORAGE_VERSION, mode, theme, contrast }));
    } catch {
      // private mode: appearance just does not persist
    }
  }, [mode, theme, contrast]);
  // Before the first paint, so the brand palette never flashes in late.
  useLayoutEffect(() => {
    const root = document.documentElement;
    root.dataset.contrast = contrast;
    root.dataset.kTheme = theme;
  }, [theme, contrast]);

  const setTheme = useCallback((t: ThemeName) => setThemeName(t), []);
  const value = useMemo(
    () => ({ mode, theme, contrast, setMode, setTheme, setContrast }),
    [mode, theme, contrast, setTheme],
  );
  return (
    <AppearanceContext.Provider value={value}>
      <Theme theme={defined} mode={mode}>
        {children}
      </Theme>
    </AppearanceContext.Provider>
  );
}

export function useAppearance(): Appearance {
  const v = useContext(AppearanceContext);
  if (!v) throw new Error('useAppearance outside AppearanceProvider');
  return v;
}
