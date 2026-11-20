export const THEME_KEY = "kudurru-theme";
export type Scheme = "light" | "dark";
export type Theme = Scheme | "system";

export function readTheme(value: string | null | undefined): Theme {
  return value === "light" || value === "dark" ? value : "system";
}

export function saveTheme(storage: Pick<Storage, "setItem" | "removeItem">, theme: Theme): void {
  if (theme === "system") storage.removeItem(THEME_KEY);
  else storage.setItem(THEME_KEY, theme);
}

export function themeColorMedia(theme: Theme, scheme: Scheme): string {
  if (theme === "system") return `(prefers-color-scheme: ${scheme})`;
  return theme === scheme ? "all" : "not all";
}
