import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { qk } from '../api/context';
import { mockDB } from '../mocks/handlers';
import SettingsPage from '../pages/SettingsPage';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
const PNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==';
async function open() {
  mockDB().license.plan = 'enterprise';
  const rendered = renderWithApp(<SettingsPage />, { route: '/settings' });
  const edit = await screen.findByRole('button', { name: 'Edit branding' });
  await waitFor(() => expect(edit).not.toHaveAttribute('aria-disabled', 'true')); fireEvent.click(edit);
  await screen.findByRole('textbox', { name: 'Organization name' });
  return rendered;
}
const click = (name: string) => fireEvent.click(screen.getByRole('button', { name }));
function input(name: string, value: string) { fireEvent.change(screen.getByRole('textbox', { name }), { target: { value } }); }
describe('Enterprise branding editor', () => {
  it('saves one coherent patch with actual ApiProvider/CSRF then publishes public appearance', async () => {
    let calls = 0, csrf = false;
    server.use(http.patch(`${API}/settings`, async ({ request }) => {
      calls++; csrf = request.headers.get('X-Kudurru-CSRF') === 'csrf-mock-token';
      const patch = await request.json() as { branding: Record<string, string> };
      expect(Object.keys(patch.branding).sort()).toEqual(['accentColor', 'favicon', 'logoDark', 'logoLight', 'name']);
      Object.assign(mockDB().settings, patch);
      return HttpResponse.json({ scope: 'cluster', node: 'kudurru-0', settings: mockDB().settings, changed: ['branding.name'], restartRequired: [], persisted: true });
    }));
    await open(); input('Organization name', 'Example Network'); input('Accent color', '#0064E0'); click('Save branding');
    await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Organization name' })).not.toBeInTheDocument());
    expect(calls).toBe(1); expect(csrf).toBe(true); await waitFor(() => expect(document.title).toBe('Example Network'));
    expect(document.querySelector('[data-configkey="branding.logoLight"] input')).toBeNull();
  });
  it('cancels drafts and resets every field by empty string', async () => {
    mockDB().settings.branding = { name: 'Saved', logoLight: PNG, logoDark: '', favicon: PNG, accentColor: '#ff0000' };
    await open(); input('Organization name', 'Draft'); click('Cancel branding'); click('Edit branding'); expect(screen.getByRole('textbox', { name: 'Organization name' })).toHaveValue('Saved');
    click('Reset branding fields'); click('Save branding'); await waitFor(() => expect(screen.queryByRole('textbox', { name: 'Organization name' })).not.toBeInTheDocument());
    expect(mockDB().settings.branding).toEqual({ name: '', logoLight: '', logoDark: '', favicon: '', accentColor: '' });
  });
  it('does not retry a failed mutation or reflect server payloads', async () => {
    let calls = 0; server.use(http.patch(`${API}/settings`, () => { calls++; return HttpResponse.json({ error: { key: 'test', message: 'Untrusted reflection' } }, { status: 503 }); }));
    await open(); input('Organization name', 'Example'); click('Save branding');
    expect(await screen.findByText('Save not confirmed')).toBeVisible(); expect(calls).toBe(1); expect(screen.queryByText('Untrusted reflection')).not.toBeInTheDocument();
  });
  it('rejects controls, surrounding whitespace and incomplete accents before a request', async () => {
    let calls = 0; server.use(http.patch(`${API}/settings`, () => { calls++; return new HttpResponse(null, { status: 500 }); }));
    await open(); input('Organization name', ' spaced '); expect(screen.getByRole('button', { name: 'Save branding' })).toBeDisabled(); input('Organization name', 'Example'); input('Accent color', '#fff'); expect(screen.getByRole('button', { name: 'Save branding' })).toBeDisabled(); expect(calls).toBe(0);
  });
  it('denies editing without Enterprise, settings.write, or with environment locks', async () => {
    const rendered = renderWithApp(<SettingsPage />, { route: '/settings' });
    expect((await screen.findAllByText(/Custom branding requires an active Enterprise/)).length).toBeGreaterThan(0); expect(screen.getByRole('button', { name: 'Edit branding' })).toHaveAttribute('aria-disabled', 'true'); rendered.unmount();
    mockDB().license.plan = 'enterprise'; mockDB().schema.find((k) => k.path === 'branding.name')!.envLocked = true;
    const next = renderWithApp(<SettingsPage />, { route: '/settings' });
    expect((await screen.findAllByText(/KUDURRU_BRANDING_NAME|KUDURRU_BRANDING__NAME/)).length).toBeGreaterThan(0); expect(screen.getByRole('button', { name: 'Edit branding' })).toHaveAttribute('aria-disabled', 'true'); next.unmount();
    mockDB().schema.find((k) => k.path === 'branding.name')!.envLocked = false;
    renderWithApp(<SettingsPage />, { route: '/settings', user: 'viewer' });
    expect((await screen.findAllByText('Needs the settings.write permission.')).length).toBeGreaterThan(0); expect(screen.getByRole('button', { name: 'Edit branding' })).toHaveAttribute('aria-disabled', 'true');
  });
  it('discards late file reads after Cancel and blocks save while reading', async () => {
    await open(); let finish!: (b: ArrayBuffer) => void;
    const file = new File(['x'], 'logo.png', { type: 'image/png' });
    Object.defineProperty(file, 'arrayBuffer', { value: () => new Promise<ArrayBuffer>((r) => { finish = r; }) });
    const picker = screen.getByLabelText('Light logo', { selector: 'input' });
    fireEvent.change(picker, { target: { files: [file] } });
    expect(screen.getByRole('button', { name: 'Save branding' })).toBeDisabled();
    click('Cancel branding'); click('Edit branding');
    await act(async () => { finish(Uint8Array.from(atob(PNG.split(',')[1]!), (c) => c.charCodeAt(0)).buffer); await Promise.resolve(); });
    expect(screen.queryByRole('img', { name: 'Light logo preview' })).not.toBeInTheDocument();
  });
  it('makes branding discoverable in Expert search without base64 text fields', async () => {
    await open(); click('Cancel branding'); fireEvent.click(screen.getByRole('radio', { name: 'Expert' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Search settings' }), { target: { value: 'branding' } });
    expect(screen.getByRole('heading', { name: 'Branding' })).toBeVisible(); expect(document.querySelector('[data-configkey="branding.logoLight"] input')).toBeNull();
  });
  it('does not apply a late save after editor lifetime ends', async () => {
    let finish!: () => void; let started = false;
    const wait = new Promise<void>((r) => { finish = r; });
    server.use(http.patch(`${API}/settings`, async () => { started = true; await wait; return HttpResponse.json({ settings: { branding: { name: 'Late' } }, scope: 'cluster' }); }));
    const { client, unmount } = await open(); input('Organization name', 'Late'); click('Save branding'); await waitFor(() => expect(started).toBe(true));
    const previous = client.getQueryData(qk.settings); unmount(); await act(async () => { finish(); await new Promise((r) => setTimeout(r, 10)); });
    expect(client.getQueryData(qk.settings)).toBe(previous); expect(document.title).not.toBe('Late');
  });
});
