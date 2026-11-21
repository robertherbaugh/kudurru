// Draws the ```mermaid blocks of a page (render-codeblock-mermaid.html) with
// the vendored bundle (docs-site/vendor/mermaid), which baseof.html loads before
// this file only on pages that have one. Light or dark follows the reader's
// color scheme, like the rest of the site.
(() => {
  const mermaid = window.mermaid;
  if (!mermaid) return;
  const nodes = [...document.querySelectorAll('.prose pre.mermaid')];
  const sources = nodes.map((node) => node.textContent);
  let requested;
  let drawing = Promise.resolve();
  const draw = (theme) => {
    requested = theme;
    drawing = drawing.then(async () => {
      if (theme !== requested) return;
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: 'strict',
        theme: theme === 'dark' ? 'dark' : 'neutral',
        ...(theme === 'dark' ? { themeVariables: { edgeLabelBackground: '#22332f' } } : {}),
        fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
      });
      nodes.forEach((node, index) => {
        node.removeAttribute('data-processed');
        node.textContent = sources[index];
      });
      await mermaid.run({ nodes, suppressErrors: true });
      for (const node of nodes) {
        const diagram = node.querySelector('svg');
        if (diagram?.viewBox.baseVal.width) {
          diagram.style.setProperty('--mermaid-width', `${diagram.viewBox.baseVal.width}px`);
        }
      }
    });
  };
  document.addEventListener('kudurru:themechange', ({ detail }) => draw(detail));
  draw(getComputedStyle(document.documentElement).colorScheme);
})();
