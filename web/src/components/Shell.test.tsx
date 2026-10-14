import { fireEvent, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderWithApp } from '../test/utils';
import { Shell } from './Shell';

async function renderShell() {
  renderWithApp(<Shell />, { route: '/tools' });
  await screen.findByRole('heading', { level: 1, name: 'Tools' });
  return screen.getByRole('navigation', { name: 'Side navigation' });
}

describe('Shell navigation', () => {
  it('starts expanded and keeps the icon rail after navigating', async () => {
    const nav = await renderShell();
    const tools = within(nav).getByRole('link', { name: 'Tools' });
    expect(tools).toHaveTextContent('Tools');
    expect(tools).toHaveAttribute('aria-current', 'page');
    expect(within(nav).getByRole('region', { name: 'Status' })).toBeInTheDocument();

    fireEvent.click(within(nav).getByRole('button', { name: 'Collapse sidebar' }));
    expect(tools).toHaveTextContent('');
    expect(within(nav).queryByRole('region', { name: 'Status' })).not.toBeInTheDocument();
    expect(within(nav).getByRole('button', { name: 'Expand sidebar' })).toHaveAttribute('aria-expanded', 'false');

    // Keyboard users can activate each named icon without needing hover.
    fireEvent.click(within(nav).getByRole('link', { name: 'Query log' }));
    await screen.findByRole('heading', { level: 1, name: 'Query log' });
    const queries = within(nav).getByRole('link', { name: 'Query log' });
    expect(queries).toHaveTextContent('');
    expect(queries).toHaveAttribute('aria-current', 'page');
  });

  it('reveals the collapsed nav on hover and lets the user pin it open', async () => {
    const user = userEvent.setup();
    const nav = await renderShell();
    fireEvent.click(within(nav).getByRole('button', { name: 'Collapse sidebar' }));

    await user.hover(nav);
    expect(within(nav).getByRole('link', { name: 'Tools' })).toHaveTextContent('Tools');
    expect(within(nav).getByRole('region', { name: 'Status' })).toBeInTheDocument();
    await user.unhover(nav);
    expect(within(nav).getByRole('link', { name: 'Tools' })).toHaveTextContent('');

    await user.hover(nav);
    await user.click(within(nav).getByRole('button', { name: 'Expand sidebar' }));
    await user.unhover(nav);
    expect(within(nav).getByRole('link', { name: 'Tools' })).toHaveTextContent('Tools');
    expect(within(nav).getByRole('button', { name: 'Collapse sidebar' })).toHaveAttribute('aria-expanded', 'true');
  });

  it('keeps the flyout open while a control inside has focus', async () => {
    const user = userEvent.setup();
    const nav = await renderShell();
    fireEvent.click(within(nav).getByRole('button', { name: 'Collapse sidebar' }));
    await user.hover(nav);
    const expand = within(nav).getByRole('button', { name: 'Expand sidebar' });
    fireEvent.focusIn(expand);
    await user.unhover(nav);
    expect(within(nav).getByRole('link', { name: 'Tools' })).toHaveTextContent('Tools');

    fireEvent.focusOut(expand, { relatedTarget: screen.getByRole('main') });
    expect(within(nav).getByRole('link', { name: 'Tools' })).toHaveTextContent('');
  });
});
