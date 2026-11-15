import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { BrandLogo } from './BrandLogo';
const PNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==';
function response(extra = {}) { server.use(http.get(`${API}/branding`, () => HttpResponse.json({ enabled: true, name: 'Example Network', logoLight: '', logoDark: '', favicon: '', accentColor: '', validUntil: new Date(Date.now() + 60_000).toISOString(), ...extra }))); }
describe('captured brand logo', () => {
  it('uses a single custom PNG for either mode and preserves safe alt text', async () => { response({ logoLight: PNG, name: '<Example>' }); renderWithApp(<BrandLogo />, { user: null }); await waitFor(() => expect(screen.getAllByRole('img', { name: '<Example>' })).toHaveLength(2)); for (const img of screen.getAllByRole('img')) { expect(img).toHaveAttribute('src', PNG); expect(img).toHaveClass('k-brand-custom'); } });
  it('falls back to organization text when an image fails without an error loop', async () => { response({ logoLight: PNG }); renderWithApp(<BrandLogo />, { user: null }); await waitFor(() => expect(screen.getAllByRole('img')[0]).toHaveAttribute('src', PNG)); fireEvent.error(screen.getAllByRole('img')[0]!); expect(screen.getByText('Example Network')).toBeVisible(); expect(screen.queryByRole('img')).not.toBeInTheDocument(); });
  it('uses text for name-only customization', async () => { response(); renderWithApp(<BrandLogo tagline />, { user: null }); expect(await screen.findByText('Example Network')).toBeVisible(); });
  it('never renders external images from a malformed public result', async () => { response({ logoLight: 'https://outside.invalid/logo.png' }); renderWithApp(<BrandLogo />, { user: null }); await waitFor(() => expect(screen.getAllByRole('img').every((i) => !i.getAttribute('src')?.startsWith('https:'))).toBe(true)); });
});
