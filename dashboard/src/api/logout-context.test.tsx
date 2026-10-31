import { act, renderHook } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { describe, expect, it } from 'vitest';
import { ApiProvider, qk, useApiContext, useSignOut } from './context';
import { server } from '../test/setup';
import { API } from '../test/utils';

describe('browser sign-out hook', () => {
  it.each(['local', 'provider-unavailable', 'untrusted-redirect'])('clears cached identity and CSRF after %s', async (mode) => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'original' } });
    queryClient.setQueryData(['private', 'answers'], ['sensitive']);
    let calls = 0;
    let sentOriginalCSRF = false;
    server.use(http.post(`${API}/auth/logout/browser`, ({ request }) => {
      calls++;
      sentOriginalCSRF = request.headers.get('X-Kudurru-CSRF') === 'original-csrf';
      return mode === 'provider-unavailable'
        ? HttpResponse.json({ error: { key: 'unavailable', message: 'Unavailable' } }, { status: 503 })
        : HttpResponse.json(mode === 'untrusted-redirect' ? { continuePath: '//foreign.example/end' } : {});
    }));
    function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}><ApiProvider baseUrl={API}>{children}</ApiProvider></QueryClientProvider>;
    }
    const hook = renderHook(() => ({ signOut: useSignOut(), context: useApiContext() }), { wrapper: Wrapper });
    hook.result.current.context.setCsrf('original-csrf');
    await act(async () => {
      try { await hook.result.current.signOut(); } catch { expect(mode).toBe('provider-unavailable'); }
    });
    expect(calls).toBe(1);
    expect(sentOriginalCSRF).toBe(true);
    expect(queryClient.getQueryData(qk.me)).toBeNull();
    expect(queryClient.getQueryData(['private', 'answers'])).toBeUndefined();
    let retainedCSRF = false;
    server.use(http.post(`${API}/probe-logout-csrf`, ({ request }) => {
      retainedCSRF = request.headers.has('X-Kudurru-CSRF');
      return new HttpResponse(null, { status: 204 });
    }));
    await hook.result.current.context.client.request('POST', '/probe-logout-csrf');
    expect(retainedCSRF).toBe(false);
  });
});
