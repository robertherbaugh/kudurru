// The "Attach" action of an unattached policy: the YAML snippet (the list
// merged into the field the policy already has, or the bare ref when the
// policy cannot be read), the wording per owner, and the dialog opened from
// the Denylist's warning banner (mock: iot-strict, managed by
// file:objects.d/policies.yaml, lacks ui-deny).
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { Policy } from '../../api/types';
import DenylistPage from '../../pages/DenylistPage';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { attachPlan, attachWhere } from './AttachDialog';

const policy = (spec: Policy['spec'], ns = 'default'): Policy => ({ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name: 'iot-strict', namespace: ns }, spec });

describe('attachPlan', () => {
  it('appends a deny list to blockListRefs, keeping the refs the policy has', () => {
    const p = attachPlan({ list: 'default/ui-deny', policy: 'default/iot-strict' }, policy({ clientGroupRefs: ['iot'], blockListRefs: ['stevenblack', 'regex-trackers'] }));
    expect(p.field).toBe('blockListRefs');
    expect(p.ref).toBe('ui-deny');
    expect(p.refs).toEqual(['stevenblack', 'regex-trackers', 'ui-deny']);
    expect(p.yaml).toBe('spec:\n  blockListRefs:\n  - stevenblack\n  - regex-trackers\n  - ui-deny\n');
    expect(p.line).toBe(5);
    expect(p.present).toBe(false);
    expect(p.kubectl).toBe(`kubectl -n default patch policies.kudurru.io iot-strict --type merge -p '{"spec":{"blockListRefs":["stevenblack","regex-trackers","ui-deny"]}}'`);
  });

  it('puts allow lists (also group-scoped ones) in allowListRefs, creating the field', () => {
    const p = attachPlan({ list: 'default/ui-allow-iot', policy: 'default/iot-strict' }, policy({ clientGroupRefs: ['iot'] }));
    expect(p.field).toBe('allowListRefs');
    expect(p.yaml).toBe('spec:\n  allowListRefs:\n  - ui-allow-iot\n');
    expect(p.line).toBe(3);
  });

  it('names the list by its bare name in the policy namespace and qualifies it elsewhere', () => {
    expect(attachPlan({ list: 'lab/ui-allow', policy: 'lab/p' }).ref).toBe('ui-allow');
    expect(attachPlan({ list: 'lab/ui-allow', policy: 'default/p' }).ref).toBe('lab/ui-allow');
  });

  it('without the policy: the new ref under a comment that keeps the existing ones, no kubectl patch', () => {
    const p = attachPlan({ list: 'default/ui-deny', policy: 'default/iot-strict' }, null);
    expect(p.refs).toBeNull();
    expect(p.yaml).toBe('spec:\n  blockListRefs:\n  # keep the entries the policy already has here, then add:\n  - ui-deny\n');
    expect(p.line).toBe(4);
    expect(p.kubectl).toBeNull();
  });

  it('notices a policy that lists the list already', () => {
    const p = attachPlan({ list: 'default/ui-deny', policy: 'default/iot-strict' }, policy({ blockListRefs: ['ui-deny'] }));
    expect(p.present).toBe(true);
    expect(p.refs).toEqual(['ui-deny']);
  });
});

describe('attachWhere', () => {
  const text = (managedBy: string) => {
    const w = attachWhere(managedBy, 'default/iot-strict', 'blockListRefs');
    const { container } = render(<>{w.lead}</>);
    return { lead: container.textContent ?? '', after: w.after };
  };

  it('a file of the objects directory: edit it on the host, applied without a restart', () => {
    const w = text('file:objects.d/policies.yaml');
    expect(w.lead).toContain('iot-strict is defined in objects.d/policies.yaml on the kudurru host');
    expect(w.lead).toContain('set spec.blockListRefs to:');
    expect(w.after).toMatch(/watches the directory .* without a restart/);
  });

  it('objects: of the configuration file: valid edits are watched', () => {
    const w = text('file:kudurru.yaml');
    expect(w.lead).toContain('under objects: in kudurru.yaml');
    expect(w.after).toMatch(/watches the configuration file .* without a restart/);
  });

  it('Kubernetes: kubectl or the GitOps manifest', () => {
    const w = text('kubernetes');
    expect(w.lead).toContain('kubectl edit policies.kudurru.io iot-strict -n default');
    expect(w.after).toMatch(/GitOps/);
  });

  it('an integration: where the integration takes its policies from', () => {
    expect(text('integration:homeassistant').lead).toContain('written by the homeassistant integration');
  });
});

describe('AttachDialog', () => {
  async function openAttach() {
    renderWithApp(<DenylistPage />, { route: '/denylist' });
    fireEvent.click(await screen.findByRole('button', { name: 'Attach ui-deny to iot-strict' }));
    return screen.findByRole('dialog', { name: 'Attach ui-deny to iot-strict' });
  }

  it('opens from the warning banner with the file, the merged YAML and its copy button', async () => {
    const dialog = await openAttach();
    expect(within(dialog).getByText('Managed by file:objects.d/policies.yaml')).toBeInTheDocument();
    const yaml = await within(dialog).findByTestId('attach-yaml');
    expect(yaml).toHaveTextContent(/spec:\s*blockListRefs:\s*- stevenblack\s*- regex-trackers\s*- ui-deny/);
    expect(within(yaml).getByRole('button', { name: /copy/i })).toBeInTheDocument();
    expect(dialog).toHaveTextContent('iot-strict is defined in objects.d/policies.yaml on the kudurru host');
    expect(dialog).toHaveTextContent(/applies the saved file within seconds/);
    // Not Kubernetes: no kubectl patch.
    expect(within(dialog).queryByTestId('attach-kubectl')).toBeNull();
    fireEvent.click(within(dialog).getAllByRole('button', { name: 'Close' }).at(-1) as HTMLElement);
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('without policies.read: the new ref and a note to keep the existing ones', async () => {
    server.use(http.get(`${API}/objects/policies/default/iot-strict`, () => HttpResponse.json({ error: { key: 'forbidden', message: 'needs policies.read' } }, { status: 403 })));
    const dialog = await openAttach();
    const yaml = await within(dialog).findByTestId('attach-yaml');
    expect(yaml).toHaveTextContent(/# keep the entries the policy already has here, then add:\s*- ui-deny/);
    expect(yaml).not.toHaveTextContent('stevenblack');
    expect(dialog).toHaveTextContent('Reading the policy needs policies.read');
  });
});
