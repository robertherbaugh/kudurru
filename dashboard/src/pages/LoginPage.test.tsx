import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { expect, it } from 'vitest';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { LoginPage } from './LoginPage';

it('sends the authenticator code intact once and clears it after rejection', async () => {
  const requests: unknown[] = [];
  server.use(http.post(`${API}/auth/login`, async ({ request }) => {
    requests.push(await request.json());
    return HttpResponse.json({ error: { key: 'unauthenticated', message: 'Invalid credentials' } }, { status: 401 });
  }));
  renderWithApp(<LoginPage />, { user: null });
  fireEvent.change(screen.getByLabelText(/^Username/), { target: { value: 'admin' } });
  fireEvent.change(screen.getByLabelText(/^Password/), { target: { value: 'test-password' } });
  const code = screen.getByLabelText(/^Authenticator code/);
  expect(code).toHaveAttribute('autocomplete', 'one-time-code');
  fireEvent.change(code, { target: { value: '12345' } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  expect(await screen.findByText('Enter the six-digit authenticator code')).toBeInTheDocument();
  expect(requests).toHaveLength(0);
  fireEvent.change(code, { target: { value: '012345' } });
  fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
  expect(await screen.findByText('Invalid credentials')).toBeInTheDocument();
  await waitFor(() => expect(code).toHaveValue(''));
  expect(requests).toEqual([{ username: 'admin', password: 'test-password', totpCode: '012345' }]);
});
