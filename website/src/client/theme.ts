import { readTheme, saveTheme, themeColorMedia, type Theme } from "./theme-preference.ts";

export function initTheme(): void {
  const root = document.documentElement;
  const control = document.querySelector<HTMLElement>(".theme-switch");
  if (!control) return;
  const options = control.querySelectorAll<HTMLInputElement>("input[type=radio]");
  const colors = document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"][data-scheme]');
  const update = (theme: Theme) => {
    if (theme !== "system") root.dataset.theme = theme;
    else delete root.dataset.theme;
    for (const option of options) option.checked = option.value === theme;
    for (const meta of colors) {
      const scheme = readTheme(meta.dataset.scheme);
      if (scheme !== "system") meta.media = themeColorMedia(theme, scheme);
    }
  };
  update(readTheme(root.dataset.theme));
  control.addEventListener("change", (event) => {
    if (!(event.target instanceof HTMLInputElement)) return;
    const theme = readTheme(event.target.value);
    update(theme);
    try {
      saveTheme(localStorage, theme);
    } catch {}
  });
  control.hidden = false;
}
