// Draws the ```mermaid blocks of a page (render-codeblock-mermaid.html) with
// the vendored bundle (docs-site/vendor/mermaid), which baseof.html loads before
// this file only on pages that have one. Light or dark follows the reader's
// colour scheme, like the rest of the site.
(() => {
  const mermaid = window.mermaid;
  if (!mermaid) return;
  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches;
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: 'strict',
    theme: dark ? 'dark' : 'neutral',
    fontFamily: 'Inter, ui-sans-serif, system-ui, sans-serif',
  });
  // suppressErrors: a diagram that fails shows Mermaid's error graphic and
  // the page's other diagrams still draw.
  mermaid.run({ querySelector: '.prose pre.mermaid', suppressErrors: true });
})();
