// Entry point of the license portal UI. Styles are prebuilt CSS files (the
// Astryx reset and core; the built kudurru theme comes with
// ui/src/astryx-themes.ts) plus styles.css; nothing is injected inline, so
// the page runs under the strict CSP the Bun server sends. In `vite dev`
// only, `?mock` serves the pages from the fixture API in ui/src/dev
// (screenshots without a server); production builds drop that code.
import '@astryxdesign/core/reset.css';
import '@astryxdesign/core/astryx.css';
import './styles.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { detectBasePath } from './ui/lib/basePath';

async function boot(): Promise<void> {
  const basePath = detectBasePath();
  if (import.meta.env.DEV && new URLSearchParams(window.location.search).has('mock')) {
    const { startDevMock } = await import('./dev/mock');
    startDevMock();
  }
  const el = document.getElementById('root');
  if (!el) throw new Error('kudurru license portal: #root is missing from index.html');
  createRoot(el).render(
    <StrictMode>
      <App basePath={basePath} />
    </StrictMode>,
  );
}

void boot();
