(() => {
  const explorer = document.getElementById('scalar-explorer');
  if (!explorer || !window.Scalar) return;

  const configuration = (theme) => ({
    url: explorer.dataset.url,
    withDefaultFonts: false,
    telemetry: false,
    agent: { disabled: true },
    hideTestRequestButton: true,
    hideClientButton: true,
    darkMode: theme === 'dark',
    forceDarkModeState: theme,
    hideDarkModeToggle: true,
  });
  const setMode = (theme) => {
    document.body.classList.toggle('dark-mode', theme === 'dark');
    document.body.classList.toggle('light-mode', theme === 'light');
  };
  const initial = getComputedStyle(document.documentElement).colorScheme;
  setMode(initial);
  const reference = window.Scalar.createApiReference(explorer, configuration(initial));
  document.addEventListener('kudurru:themechange', ({ detail: theme }) => {
    reference.updateConfiguration(configuration(theme));
    setMode(theme);
  });
})();
