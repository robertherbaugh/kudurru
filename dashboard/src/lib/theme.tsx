// Appearance: the Astryx themes of src/themes with auto/light/dark modes
// and a high-contrast setting. The default, Kudurru, is Astryx Neutral
// with the branding palette (src/themes/kudurru); Neutral, Stone and the
// fun Y2K are the Astryx themes with kudurru's query status colours. All
// four are built themes wired through the generated src/astryx-themes.ts
// (`bun run astryx -- theme add <slug> --import`), so no theme injects
// <style> at runtime and the strict CSP holds.
import { createContext, useCallback, useContext, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import { defaultThemeSlug, themes, type ThemeSlug } from '../astryx-themes';

export type Mode = 'system' | 'light' | 'dark';
export type ThemeName = ThemeSlug;
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
  const fallback = { mode: 'system' as Mode, theme: defaultThemeSlug, contrast: 'normal' as Contrast };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<typeof fallback> & { v?: number };
    const picked = v.theme === 'stone' || v.theme === 'y2k' || (v.theme === 'neutral' && v.v === STORAGE_VERSION);
    return {
      mode: v.mode === 'light' || v.mode === 'dark' ? v.mode : 'system',
      theme: picked ? (v.theme as ThemeName) : defaultThemeSlug,
      contrast: v.contrast === 'more' ? 'more' : 'normal',
    };
  } catch {
    return fallback;
  }
}

export function AppearanceProvider({ children }: { children: ReactNode }) {
  const initial = useMemo(load, []);
  const [mode, setMode] = useState<Mode>(initial.mode);
  const [theme, setThemeName] = useState<ThemeName>(initial.theme);
  const [contrast, setContrast] = useState<Contrast>(initial.contrast);

  useEffect(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ v: STORAGE_VERSION, mode, theme, contrast }));
    } catch {
      // private mode: appearance just does not persist
    }
  }, [mode, theme, contrast]);
  // High contrast switches the query colours to the themes' -strong
  // variants and strengthens borders and secondary text (styles.css,
  // keyed on <html data-contrast>). Before the first paint, so it never
  // flashes in late.
  useLayoutEffect(() => {
    document.documentElement.dataset.contrast = contrast;
  }, [contrast]);

  const setTheme = useCallback((t: ThemeName) => setThemeName(t), []);
  const value = useMemo(
    () => ({ mode, theme, contrast, setMode, setTheme, setContrast }),
    [mode, theme, contrast, setTheme],
  );
  return (
    <AppearanceContext.Provider value={value}>
      <Theme theme={themes[theme]} mode={mode}>
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
