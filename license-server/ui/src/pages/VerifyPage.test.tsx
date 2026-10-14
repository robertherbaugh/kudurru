// The /verify page: reachable without a session (it never asks for one),
// confirms only on a click, and words each outcome and each failure; the
// usual outcome is a request waiting for a license admin's approval.
import { fireEvent, screen, waitFor } from '@testing-library/react';
import type { PersonalOutcome } from '@shared/api';
import { apiError } from '../dev/api';
import { renderApp } from '../test/render';
import { outcomeWords } from '../lib/words';

const outcomes: PersonalOutcome[] = ['issued', 'renewed', 'resent', 'pending_review'];

test.each(outcomes)('outcome %s is explained after the click', async (outcome) => {
  const { api } = renderApp({ route: '/verify?token=tok_123', me: null, handlers: { publicKeyRequestVerify: () => ({ outcome }) } });
  const button = await screen.findByRole('button', { name: 'Confirm my request' });
  // Nothing is consumed by opening the link.
  expect(api.callsTo('publicKeyRequestVerify')).toHaveLength(0);
  fireEvent.click(button);
  const words = outcomeWords(outcome, null, 'web');
  expect(await screen.findByText(words.title)).toBeInTheDocument();
  expect(screen.getByText(words.body)).toBeInTheDocument();
  // A key from the form reaches a portal account through the mail's invite link, not by signing in alone.
  if (outcome === 'issued') expect(words.body).toMatch(/link that adds the key to your account/);
  expect(api.callsTo('publicKeyRequestVerify')[0]?.body).toEqual({ token: 'tok_123' });
  // Outside the session gate: no session was asked for, no CSRF header sent.
  expect(api.callsTo('authMe')).toHaveLength(0);
  expect(api.callsTo('publicKeyRequestVerify')[0]?.headers.get('X-Kudurru-CSRF')).toBeNull();
});

test('a request waiting for approval says the key arrives by email once it is approved', async () => {
  renderApp({ route: '/verify?token=tok_123', me: null, handlers: { publicKeyRequestVerify: () => ({ outcome: 'pending_review' }) } });
  // Before the click the page already says that approval comes first.
  expect(await screen.findByText(/the key arrives by email at the address you gave once it is approved/)).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Confirm my request' }));
  expect(await screen.findByText('Your request is waiting for approval')).toBeInTheDocument();
  expect(screen.getByText('Your key arrives by email once it is approved.')).toBeInTheDocument();
  expect(screen.queryByText(/key is ready|mailed the key/)).not.toBeInTheDocument();
});

test('a used or expired link is explained, with the way to ask again', async () => {
  renderApp({ route: '/verify?token=tok_old', me: null, handlers: { publicKeyRequestVerify: () => apiError(400, 'invalid_token', 'Invalid token') } });
  fireEvent.click(await screen.findByRole('button', { name: 'Confirm my request' }));
  expect(await screen.findByText('This confirmation link no longer works')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: /Request a key/ })).toHaveAttribute('href', 'https://kudurru.io/license');
  await waitFor(() => expect(screen.getByRole('button', { name: 'Confirm my request' })).toBeDisabled());
});

test('a rate limit says when to try again and keeps the button', async () => {
  renderApp({ route: '/verify?token=tok_1', me: null, handlers: { publicKeyRequestVerify: () => ({ ...apiError(429, 'rate_limited', 'Too many requests'), headers: { 'Retry-After': '30' } }) } });
  fireEvent.click(await screen.findByRole('button', { name: 'Confirm my request' }));
  expect(await screen.findByText('Too many attempts')).toBeInTheDocument();
  expect(screen.getByText(/try again in 30 seconds/)).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Confirm my request' })).toBeEnabled();
});

test('without a token the page asks for the mailed link', async () => {
  const { api } = renderApp({ route: '/verify', me: null });
  expect(await screen.findByText('This page needs the link from your mail')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Confirm my request' })).not.toBeInTheDocument();
  expect(api.calls).toHaveLength(0);
});
