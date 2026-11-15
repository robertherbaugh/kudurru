import { Agent, getGlobalDispatcher, setGlobalDispatcher } from 'undici';

/** Own the native fetch connections for one MSW fixture. */
export function installTestTransport(): () => Promise<void> {
  const previous = getGlobalDispatcher();
  // MSW's socket interceptor can pass an idle replacement connection opened
  // after cancellation through to the real network before HTTP bytes arrive.
  // Keep each mocked exchange on its own connection, including native fetch.
  // https://github.com/mswjs/interceptors/issues/862
  const dispatcher = new Agent({ pipelining: 0 });
  setGlobalDispatcher(dispatcher);
  return async () => {
    try {
      await dispatcher.destroy();
    } finally {
      setGlobalDispatcher(previous);
    }
  };
}
