import { THEME_KEY, readTheme } from "./theme-preference.ts";

try {
  const theme = readTheme(localStorage.getItem(THEME_KEY));
  if (theme !== "system") document.documentElement.dataset.theme = theme;
} catch {}
