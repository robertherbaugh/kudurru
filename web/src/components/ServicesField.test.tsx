import { fireEvent, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { AnyObject, CatalogueService } from '../api/types';
import { SCHEMAS } from '../lib/objectSchemas';
import { renderWithApp } from '../test/utils';
import { ObjectForm } from './ObjectForm';
import { groupServices } from './ServicesField';

const svc = (id: string, name: string, category: string): CatalogueService => ({ id, name, category, list: `service:${id}`, rules: 1, domains: [`||${id}.com^`], source: 'x' });

describe('groupServices', () => {
  it('groups by category and sorts both levels by name', () => {
    const g = groupServices([svc('tiktok', 'TikTok', 'social'), svc('youtube', 'YouTube', 'video'), svc('netflix', 'Netflix', 'video')]);
    expect(g.map(([c, l]) => [c, l.map((s) => s.id)])).toEqual([
      ['social', ['tiktok']],
      ['video', ['netflix', 'youtube']],
    ]);
  });
});

describe('Policy form', () => {
  it('picks blocked services from the catalogue, keeps unknown IDs and enables SafeSearch', async () => {
    const onSubmit = vi.fn<(o: AnyObject) => Promise<void>>().mockResolvedValue(undefined);
    const initial = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'Policy',
      metadata: { name: 'kids', namespace: 'default' },
      spec: { clientGroupRefs: [], blockedServices: ['myspace'], priority: 10 },
    } as unknown as AnyObject;
    renderWithApp(<ObjectForm schema={SCHEMAS.policies} initial={initial} onSubmit={onSubmit} onCancel={() => {}} />);
    const youtube = await screen.findByRole('checkbox', { name: /YouTube/ });
    fireEvent.click(youtube);
    expect(screen.getByText('Not in the catalogue (ignored until it is)')).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'myspace' })).toBeChecked();
    fireEvent.click(screen.getByRole('switch', { name: /Enforce SafeSearch/ }));
    fireEvent.change(screen.getByLabelText(/SafeSearch level/), { target: { value: 'moderate' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    const spec = (onSubmit.mock.calls[0]?.[0] as { spec: Record<string, unknown> }).spec;
    expect(spec.blockedServices).toEqual(['myspace', 'youtube']);
    expect(spec.safeSearch).toBe(true);
    expect(spec.safeSearchMode).toBe('moderate');
  });
});
