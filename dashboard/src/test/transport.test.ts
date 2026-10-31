import { createServer } from 'node:net';
import { http, HttpResponse } from 'msw';
import { expect, it } from 'vitest';
import { server } from './setup';

const turn = () => new Promise<void>((resolve) => setImmediate(resolve));

it('keeps canceled native fetches and subsequent one-shot reads inside MSW', async () => {
  let realConnections = 0;
  const sink = createServer((socket) => { realConnections++; socket.destroy(); });
  await new Promise<void>((resolve) => sink.listen(0, resolve));
  const address = sink.address();
  if (!address || typeof address === 'string') throw new Error('Missing fixture listener');
  const origin = `http://localhost:${address.port}`;
  let reads = 0;
  try {
    for (let phase = 0; phase < 6; phase++) {
      let started = 0;
      let ready!: () => void;
      const bothStarted = new Promise<void>((resolve) => { ready = resolve; });
      server.use(
        http.get(`${origin}/held/:n`, ({ request }) => new Promise<Response>((resolve) => {
          request.signal.addEventListener('abort', () => resolve(HttpResponse.error()), { once: true });
          if (++started === 2) ready();
        })),
        http.get(`${origin}/status`, async () => {
          reads++;
          await turn(); await turn();
          return HttpResponse.json({ enabled: false });
        }),
      );
      const controller = new AbortController();
      const pending = [1, 2].map((n) => fetch(`${origin}/held/${n}`, { signal: controller.signal }).catch((error: unknown) => error));
      await bothStarted;
      controller.abort();
      // Exercise each ordering around the interceptor's read-before-write
      // setImmediate and the canceled socket's close/reconnect callbacks.
      for (let i = 0; i < phase; i++) await turn();
      const response = await fetch(`${origin}/status`);
      expect(response.status).toBe(200);
      expect(await response.json()).toEqual({ enabled: false });
      for (const error of await Promise.all(pending)) expect(error).toMatchObject({ name: 'AbortError' });
    }
  } finally {
    await new Promise<void>((resolve, reject) => sink.close((error) => error ? reject(error) : resolve()));
  }
  expect(reads).toBe(6);
  expect(realConnections).toBe(0);
});
