// Vitest setup for the UI tests: jest-dom matchers, cleanup between tests
// and the browser APIs jsdom lacks. A placeholder from the Phase 0 tooling;
// stream C replaces it with the copy of web/src/test/setup.ts.
import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { afterEach } from 'vitest';

// CI runners are slow; findBy* waits up to 5 s.
configure({ asyncUtilTimeout: 5000 });

afterEach(() => {
  cleanup();
});
