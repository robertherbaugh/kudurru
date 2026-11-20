export function initTheme(): void {
  const root = document.documentElement;
  const control = document.querySelector<HTMLElement>(".theme-switch");
  if (!control) return;
  const options = control.querySelectorAll<HTMLInputElement>("input[type=radio]");
  const colors = document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"][media]');
  const update = (theme: string) => {
    if (theme === "light" || theme === "dark") root.dataset.theme = theme;
    else delete root.dataset.theme;
    for (const option of options) option.checked = option.value === theme;
    for (const meta of colors) {
      const scheme = meta.content === "#182522" ? "dark" : "light";
      meta.media = theme === "system" ? `(prefers-color-scheme: ${scheme})` : theme === scheme ? "all" : "not all";
    }
  };
  update(root.dataset.theme ?? "system");
  control.addEventListener("change", (event) => {
    if (!(event.target instanceof HTMLInputElement)) return;
    const theme = event.target.value;
    update(theme);
    try {
      if (theme === "system") localStorage.removeItem("kudurru-theme");
      else localStorage.setItem("kudurru-theme", theme);
    } catch {}
  });
  control.hidden = false;
}
