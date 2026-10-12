import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { renderWithApp } from '../test/utils';
import { ScopeNote } from './ScopeSelector';

describe('ScopeNote', () => {
  it('names the nodes of a cluster answer', () => {
    renderWithApp(<ScopeNote scope={{ scope: 'cluster', nodes: [{ name: 'a', ok: true }, { name: 'b', ok: true }], partial: false }} />);
    expect(screen.getByTestId('scope-note').textContent).toBe('All nodes: a, b');
  });

  it('names the central database', () => {
    renderWithApp(<ScopeNote scope={{ scope: 'cluster', nodes: [{ name: 'a', ok: true }, { name: 'b', ok: false, error: 'statistics last received 6m0s ago' }], partial: true, source: 'central' }} />);
    expect(screen.getByTestId('scope-note').textContent).toBe('All nodes: a, b (central database) (partial: b did not answer)');
  });
});
