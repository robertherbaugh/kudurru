// Entry point. Styles are prebuilt CSS files (the Astryx reset and core,
// uPlot; the built themes come with src/astryx-themes.ts) plus
// styles.css; nothing is injected inline, so the page runs under the
// strict CSP web.Handler sends.
import '@astryxdesign/core/reset.css';
import '@astryxdesign/core/astryx.css';
import 'uplot/dist/uPlot.min.css';
import './styles.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { detectBasePath } from './lib/basePath';

async function boot(): Promise<void> {
  const basePath = detectBasePath();
  if (import.meta.env.KUDURRU_MOCK) {
    const { startMockWorker } = await import('./mocks/browser');
    await startMockWorker(basePath);
  }
  const el = document.getElementById('root');
  if (!el) throw new Error('kudurru: #root is missing from index.html');
  createRoot(el).render(
    <StrictMode>
      <App basePath={basePath} />
    </StrictMode>,
  );
}

void boot();
