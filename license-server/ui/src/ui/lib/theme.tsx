// Copied from dashboard/src/lib/theme.tsx at dd3dcb0; keep in step by hand.
// Changes: one theme (kudurru) from ui/src/astryx-themes.ts, so no theme
// list, no theme setter and no storage migration; its own storage key
// (kudurru-license-portal.appearance) so the portal and a dashboard on the
// same origin do not share settings.
//
// Appearance: the kudurru theme (Astryx Neutral with the branding
// palette, ui/src/themes/kudurru) with auto/light/dark modes and a
// high-contrast setting. The theme is a built theme wired through the
// generated ui/src/astryx-themes.ts, so no theme injects <style> at runtime
// and the strict CSP holds.
import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useState, type ReactNode } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import { defaultThemeSlug, themes } from '../../astryx-themes';

export type Mode = 'system' | 'light' | 'dark';
export type Contrast = 'normal' | 'more';

interface Appearance {
  mode: Mode;
  contrast: Contrast;
  setMode: (m: Mode) => void;
  setContrast: (c: Contrast) => void;
}

const AppearanceContext = createContext<Appearance | null>(null);

const STORAGE_KEY = 'kudurru-license-portal.appearance';

function load(): { mode: Mode; contrast: Contrast } {
  const fallback = { mode: 'system' as Mode, contrast: 'normal' as Contrast };
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as Partial<typeof fallback>;
    return {
      mode: v.mode === 'light' || v.mode === 'dark' ? v.mode : 'system',
      contrast: v.contrast === 'more' ? 'more' : 'normal',
    };
  } catch {
    return fallback;
  }
}

export function AppearanceProvider({ children }: { children: ReactNode }) {
  const initial = useMemo(load, []);
  const [mode, setMode] = useState<Mode>(initial.mode);
  const [contrast, setContrast] = useState<Contrast>(initial.contrast);

  useEffect(() => {
    try {
      window.localStorage.setItem(STORAGE_KEY, JSON.stringify({ mode, contrast }));
    } catch {
      // private mode: appearance just does not persist
    }
  }, [mode, contrast]);
  // High contrast strengthens borders and secondary text (styles.css,
  // keyed on <html data-contrast>). Before the first paint, so it never
  // flashes in late.
  useLayoutEffect(() => {
    document.documentElement.dataset.contrast = contrast;
  }, [contrast]);

  const value = useMemo(() => ({ mode, contrast, setMode, setContrast }), [mode, contrast]);
  return (
    <AppearanceContext.Provider value={value}>
      <Theme theme={themes[defaultThemeSlug]} mode={mode}>
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
