import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { renderWithApp } from '../test/utils';
import { ScopeNote } from './ScopeSelector';

describe('ScopeNote', () => {
  it('summarizes a cluster answer without listing node names', () => {
    renderWithApp(<ScopeNote scope={{ scope: 'cluster', nodes: [{ name: 'a', ok: true }, { name: 'b', ok: true }], partial: false }} />);
    expect(screen.getByTestId('scope-note').textContent).toBe('All nodes (2)');
  });

  it('names the central database', () => {
    renderWithApp(<ScopeNote scope={{ scope: 'cluster', nodes: [{ name: 'a', ok: true }, { name: 'b', ok: false, error: 'statistics last received 6m0s ago' }], partial: true, source: 'central' }} />);
    expect(screen.getByTestId('scope-note').textContent).toBe('All nodes (2) (central database) · Partial results: 1 node delayed or unavailable');
  });
});
