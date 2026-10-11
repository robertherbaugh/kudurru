// Appearance: Astryx themes delivered as prebuilt CSS (D69) with
// auto/light/dark modes, a high-contrast overlay and one fun theme.
// Neutral ships in the initial bundle; Stone and Y2K are separate chunks
// loaded on first use. No theme injects <style> at runtime (built themes
// only), so the strict CSP holds.
import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import type { DefinedTheme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';

export type Mode = 'system' | 'light' | 'dark';
export type ThemeName = 'neutral' | 'stone' | 'y2k';
export type Contrast = 'normal' | 'more';

export const THEMES: { id: ThemeName; label: string }[] = [
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

function load(): { mode: Mode; theme: ThemeName; contrast: Contrast } {
  const fallback = { mode: 'system' as Mode, theme: 'neutral' as ThemeName, contrast: 'normal' as Contrast };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<typeof fallback>;
    return {
      mode: v.mode === 'light' || v.mode === 'dark' ? v.mode : 'system',
      theme: v.theme === 'stone' || v.theme === 'y2k' ? v.theme : 'neutral',
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
      return mod.stoneTheme as DefinedTheme;
    }
    case 'y2k': {
      const [mod] = await Promise.all([import('@astryxdesign/theme-y2k/built'), import('@astryxdesign/theme-y2k/theme.css')]);
      return mod.y2kTheme as DefinedTheme;
    }
    default:
      return neutralTheme as DefinedTheme;
  }
}

export function AppearanceProvider({ children }: { children: ReactNode }) {
  const initial = useMemo(load, []);
  const [mode, setMode] = useState<Mode>(initial.mode);
  const [theme, setThemeName] = useState<ThemeName>(initial.theme);
  const [contrast, setContrast] = useState<Contrast>(initial.contrast);
  const [defined, setDefined] = useState<DefinedTheme>(neutralTheme as DefinedTheme);

  useEffect(() => {
    let live = true;
    loadTheme(theme)
      .then((t) => {
        if (live) setDefined(t);
      })
      .catch(() => {
        if (live) setDefined(neutralTheme as DefinedTheme);
      });
    return () => {
      live = false;
    };
  }, [theme]);

  useEffect(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ mode, theme, contrast }));
    } catch {
      // private mode: appearance just does not persist
    }
    document.documentElement.dataset.contrast = contrast;
  }, [mode, theme, contrast]);

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
