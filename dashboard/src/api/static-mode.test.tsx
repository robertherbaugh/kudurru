import { fireEvent, renderHook, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import type { ReactNode } from 'react';
import { describe, expect, it } from 'vitest';
import { ApiProvider, qk, useCan, useStaticMode } from './context';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { AccountMenu } from '../components/AccountMenu';
import { AuthenticatorPanel } from '../components/access/Authenticators';

describe('static host capabilities', () => {
  it('keeps read and runtime controls while refusing desired-object actions', async () => {
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ readOnly: true })));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'admin' }, permissions: { '*': ['*'] } });
    function Wrapper({ children }: { children: ReactNode }) {
      return <QueryClientProvider client={queryClient}><ApiProvider baseUrl={API}>{children}</ApiProvider></QueryClientProvider>;
    }
    const hook = renderHook(() => ({ readOnly: useStaticMode(), can: useCan() }), { wrapper: Wrapper });
    await waitFor(() => expect(hook.result.current.readOnly).toBe(true));
    expect(hook.result.current.can('zones', 'read')).toBe(true);
    expect(hook.result.current.can('blocking', 'toggle')).toBe(true);
    expect(hook.result.current.can('cache', 'flush')).toBe(true);
    for (const verb of ['write', 'delete', 'decide', 'import', 'sync', 'act']) {
      expect(hook.result.current.can('zones', verb)).toBe(false);
    }
    hook.unmount();
    queryClient.clear();
  });

  it('keeps sign-out and explains account provisioning instead of offering changes', async () => {
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ readOnly: true })));
    renderWithApp(<><AccountMenu /><AuthenticatorPanel /></>);
    expect(await screen.findByText('Credentials are read-only in static mode')).toBeInTheDocument();
    const changePassword = screen.getByRole('button', { name: 'Change password' });
    expect(changePassword).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(changePassword);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Sign out' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Set up authenticator' })).not.toBeInTheDocument();
  });
});
