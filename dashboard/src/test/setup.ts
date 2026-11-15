// Vitest setup: jest-dom matchers and the browser APIs jsdom lacks.
import '@testing-library/jest-dom/vitest';
import { cleanup, configure } from '@testing-library/react';
import { setupServer } from 'msw/node';
import { afterAll, afterEach, beforeAll, beforeEach } from 'vitest';
import { handlers, resetDB } from '../mocks/handlers';
import { installTestTransport } from './transport';

// CI runners and the shared dev machine are slow; findBy* waits up to 5 s.
configure({ asyncUtilTimeout: 5000 });

/** The MSW server answering /api/v1 from the fixtures in every test. */
export const server = setupServer(...handlers);

let disposeTransport: () => Promise<void>;
beforeAll(() => {
  disposeTransport = installTestTransport();
  server.listen({ onUnhandledFrame: 'error' });
});
beforeEach(() => {
  resetDB();
  window.localStorage.clear();
  window.sessionStorage.clear();
});
afterAll(async () => {
  try {
    await disposeTransport();
  } finally {
    server.close();
  }
});

class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

if (!('ResizeObserver' in globalThis)) {
  Object.defineProperty(globalThis, 'ResizeObserver', { value: ResizeObserverStub, writable: true });
}

if (!window.matchMedia) {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => undefined,
      removeListener: () => undefined,
      addEventListener: () => undefined,
      removeEventListener: () => undefined,
      dispatchEvent: () => false,
    }),
  });
}

// jsdom logs "Not implemented" for scrolling; the app only scrolls to top.
window.scrollTo = (() => undefined);
Element.prototype.scrollTo = function scrollTo() {};
Element.prototype.scrollIntoView = function scrollIntoView() {};

// jsdom does no layout, so every offset size is 0 and TanStack Virtual
// would render no rows. Give the query-log scroller a viewport and each
// virtual row (one <tbody>) a fixed height.
const offsetSize = (axis: 'w' | 'h') =>
  function (this: HTMLElement): number {
    if (this.dataset.testid === 'query-log-scroll') return axis === 'h' ? 800 : 1200;
    if (this.tagName === 'TBODY' && this.dataset.index !== undefined) return axis === 'h' ? 34 : 1200;
    return 0;
  };
Object.defineProperty(HTMLElement.prototype, 'offsetHeight', { configurable: true, get: offsetSize('h') });
Object.defineProperty(HTMLElement.prototype, 'offsetWidth', { configurable: true, get: offsetSize('w') });

// No canvas in jsdom: charts detect the null context and skip drawing
// (their data tables and legends still render).
HTMLCanvasElement.prototype.getContext = (() => null) as typeof HTMLCanvasElement.prototype.getContext;

// jsdom has HTMLDialogElement but not its modal methods.
const dialogProto = HTMLDialogElement.prototype as unknown as Record<string, unknown>;
if (typeof dialogProto.showModal !== 'function') {
  dialogProto.showModal = function showModal(this: HTMLDialogElement) {
    this.setAttribute('open', '');
  };
  dialogProto.show = function show(this: HTMLDialogElement) {
    this.setAttribute('open', '');
  };
  dialogProto.close = function close(this: HTMLDialogElement) {
    this.removeAttribute('open');
    this.dispatchEvent(new Event('close'));
  };
}

afterEach(() => {
  cleanup();
  server.resetHandlers();
});
