// The downloads page's public destinations, copy commands and session gate.
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { vi } from 'vitest';
import { customerMe, me } from '../dev/fixtures';
import { renderApp } from '../test/render';

test('a customer finds release assets, registry packages and copyable pull commands', async () => {
  const user = userEvent.setup();
  const writeText = vi.spyOn(navigator.clipboard, 'writeText');
  const { api } = renderApp({ route: '/downloads', me: customerMe });
  expect(await screen.findByRole('heading', { level: 1, name: 'Downloads' })).toBeInTheDocument();

  const destinations = [
    ['Latest stable release', 'https://github.com/robertherbaugh/kudurru/releases/latest'],
    ['All releases', 'https://github.com/robertherbaugh/kudurru/releases'],
    ['Server chart', 'https://github.com/users/robertherbaugh/packages/container/package/charts%2Fkudurru'],
    ['CRD chart', 'https://github.com/users/robertherbaugh/packages/container/package/charts%2Fkudurru-crds'],
    ['Server images', 'https://github.com/users/robertherbaugh/packages/container/package/kudurru'],
    ['Compose release bundle', 'https://github.com/robertherbaugh/kudurru/releases/latest'],
    ['crds.yaml', 'https://github.com/robertherbaugh/kudurru/releases/latest/download/crds.yaml'],
    ['install.yaml', 'https://github.com/robertherbaugh/kudurru/releases/latest/download/install.yaml'],
    ['Latest release checksums', 'https://github.com/robertherbaugh/kudurru/releases/latest/download/checksums.txt'],
  ];
  for (const [label, href] of destinations) {
    const link = screen.getByRole('link', { name: `${label}(opens in new tab)` });
    expect(link).toHaveAttribute('href', href);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', expect.stringContaining('noopener'));
  }

  const container = screen.getByRole('region', { name: 'Pull a container image' });
  const docker = 'docker pull ghcr.io/robertherbaugh/kudurru:latest';
  expect(container).toHaveTextContent(docker);
  await user.click(within(container).getByRole('button', { name: 'Copy code' }));
  expect(writeText).toHaveBeenLastCalledWith(docker);

  const charts = screen.getByRole('region', { name: 'Download Helm charts' });
  const helm = 'helm pull oci://ghcr.io/robertherbaugh/charts/kudurru-crds\nhelm pull oci://ghcr.io/robertherbaugh/charts/kudurru';
  for (const command of helm.split('\n')) expect(charts).toHaveTextContent(command);
  await user.click(within(charts).getByRole('button', { name: 'Copy code' }));
  expect(writeText).toHaveBeenLastCalledWith(helm);
  expect(api.unhandled).toEqual([]);
});

test('a signed-in user without a role can navigate to downloads', async () => {
  renderApp({ route: '/downloads', me: me([]) });
  expect(await screen.findByRole('heading', { level: 1, name: 'Downloads' })).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Downloads' })).toHaveAttribute('href', '/downloads');
  expect(screen.queryByRole('heading', { name: 'Not permitted' })).not.toBeInTheDocument();
});

test('a signed-out visitor must sign in and returns to downloads', async () => {
  renderApp({ route: '/downloads', me: null, handlers: { authConfig: () => ({ displayName: 'Sign in with Dex', devMode: false }) } });
  expect(await screen.findByRole('link', { name: 'Sign in with Dex' })).toHaveAttribute('href', '/api/v1/auth/login?next=%2Fdownloads');
  expect(window.location.pathname).toBe('/login');
  expect(new URLSearchParams(window.location.search).get('next')).toBe('/downloads');
  expect(screen.queryByRole('heading', { name: 'Downloads' })).not.toBeInTheDocument();
});
