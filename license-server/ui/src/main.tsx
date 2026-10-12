// Entry point of the license portal UI. A placeholder from the Phase 0
// tooling (stream C replaces it with the app shell): it proves the build,
// the Astryx styles and the kudurru theme load under the strict CSP.
// Styles are prebuilt CSS files; nothing is injected inline.
import '@astryxdesign/core/reset.css';
import '@astryxdesign/core/astryx.css';
import './themes/kudurru/kudurru.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { EmptyState } from '@astryxdesign/core/EmptyState';
import { Theme } from '@astryxdesign/core/theme';
import { OFFLINE_NOTE } from '@shared/catalog';
import { kudurruTheme } from './themes/kudurru/kudurru.js';

const el = document.getElementById('root');
if (!el) throw new Error('kudurru license portal: #root is missing from index.html');
createRoot(el).render(
  <StrictMode>
    <Theme theme={kudurruTheme}>
      <EmptyState title="kudurru license portal" description={OFFLINE_NOTE} headingLevel={1} />
    </Theme>
  </StrictMode>,
);
