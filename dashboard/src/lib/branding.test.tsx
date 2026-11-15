import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { AppProviders } from '../App';
import { testQueryClient } from '../test/utils';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { brandingKey, DEFAULT_BRANDING, effectiveBranding, useBranding } from './branding';
import { useAppearance } from './theme';
const PNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGNgYGD4DwABBAEAX+XDSwAAAABJRU5ErkJggg==';
const brand = (extra = {}) => ({ enabled: true, name: 'Example Network', logoLight: '', logoDark: '', favicon: '', accentColor: '#0064E0', validUntil: new Date(Date.now() + 60_000).toISOString(), ...extra });
function Probe() { const b = useBranding(); const a = useAppearance(); return <><output aria-label="brand">{b.enabled ? b.name : 'default'}</output><output aria-label="appearance">{`${a.theme}/${a.mode}/${a.contrast}`}</output><button onClick={() => { a.setTheme('stone'); a.setMode('dark'); a.setContrast('more'); }}>Pick appearance</button></>; }
afterEach(() => { vi.restoreAllMocks(); });
describe('public captured branding', () => {
  it('loads anonymously without settings/license reads and preserves browser choices', async () => {
    let privateReads = 0;
    server.use(http.get(`${API}/branding`, () => HttpResponse.json(brand())), http.get(`${API}/settings`, () => { privateReads++; return new HttpResponse(null, { status: 403 }); }), http.get(`${API}/license`, () => { privateReads++; return new HttpResponse(null, { status: 403 }); }));
    renderWithApp(<Probe />, { user: null });
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network'));
    fireEvent.click(screen.getByRole('button', { name: 'Pick appearance' }));
    expect(screen.getByLabelText('appearance')).toHaveTextContent('stone/dark/more');
    expect(document.documentElement).toHaveAttribute('data-astryx-theme', 'organization-stone');
    expect(privateReads).toBe(0);
  });
  it('loads through a mounted API path prefix without root literals', async () => { const base = `${window.location.origin}/dashboard/api/v1`; let reads = 0; server.use(http.get(`${base}/branding`, () => { reads++; return HttpResponse.json(brand()); })); render(<AppProviders basePath="/dashboard" client={testQueryClient()} apiBase={base}><Probe /></AppProviders>); await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network')); expect(reads).toBe(1); });
  it('defaults on absent/invalid/exact-expired deadline, names, assets and accents', () => {
    for (const patch of [{ validUntil: undefined }, { validUntil: 'tomorrow' }, { validUntil: new Date().toISOString() }, { name: '\u200bhidden' }, { name: ' spaced ' }, { logoLight: 'https://outside.invalid/image.png' }, { accentColor: 'red' }]) expect(effectiveBranding(brand(patch))).toBe(DEFAULT_BRANDING);
    expect(effectiveBranding(brand({ name: '🦊'.repeat(80) }))).not.toBe(DEFAULT_BRANDING);
  });
  it('restores exact title/favicon nodes after expiry and clears effective cache', async () => {
    const title = document.title;
    const icon = document.createElement('link'); icon.rel = 'icon'; icon.href = '/original.png'; icon.setAttribute('sizes', '32x32'); document.head.append(icon);
    server.use(http.get(`${API}/branding`, () => HttpResponse.json(brand({ favicon: PNG, validUntil: new Date(Date.now() + 350).toISOString() }))));
    // Hold one exact response; subsequent public polls cannot extend its deadline.
    const response = brand({ favicon: PNG, validUntil: new Date(Date.now() + 350).toISOString() });
    server.use(http.get(`${API}/branding`, () => HttpResponse.json(response)));
    const { client, unmount } = renderWithApp(<Probe />, { user: null });
    await waitFor(() => expect(document.title).toBe('Example Network'));
    expect(icon.isConnected).toBe(false); expect(document.head.querySelector('link[rel="icon"]')).toHaveAttribute('href', PNG);
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('default'));
    expect(document.title).toBe(title); expect(icon.isConnected).toBe(true); expect(icon.getAttribute('sizes')).toBe('32x32');
    await waitFor(() => expect(effectiveBranding(client.getQueryData(brandingKey(API)))).toBe(DEFAULT_BRANDING));
    unmount(); icon.remove();
  });
  it('restores defaults immediately on a failed refresh despite old cached brand', async () => {
    let fail = false;
    server.use(http.get(`${API}/branding`, () => fail ? new HttpResponse(null, { status: 503 }) : HttpResponse.json(brand())));
    const { client } = renderWithApp(<Probe />, { user: null });
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network'));
    fail = true;
    await act(async () => { await client.invalidateQueries({ queryKey: brandingKey(API) }); });
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('default'));
  });
  it('defaults and resets stale cache when a suspended tab resumes after the deadline', async () => {
    let now = Date.now(); vi.spyOn(Date, 'now').mockImplementation(() => now);
    const response = brand({ validUntil: new Date(now + 60_000).toISOString() });
    server.use(http.get(`${API}/branding`, () => HttpResponse.json(response)));
    const { client } = renderWithApp(<Probe />, { user: null });
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network'));
    now += 60_001; fireEvent(window, new Event('focus'));
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('default'));
    await waitFor(() => expect(client.getQueryData(brandingKey(API))).toEqual(DEFAULT_BRANDING));
  });
  it('retains a long license at the bounded timer wake and reschedules the remaining lifetime', async () => {
    const now = Date.now(); let actual = now;
    vi.spyOn(Date, 'now').mockImplementation(() => actual);
    let wake: (() => void) | undefined; const delays: number[] = [];
    const original = window.setTimeout.bind(window);
    vi.spyOn(window, 'setTimeout').mockImplementation((fn: TimerHandler, delay?: number, ...args: unknown[]) => {
      if (delay && delay > 1_000_000_000) delays.push(delay);
      if (delay === 2_147_483_647 && typeof fn === 'function') { wake = fn as () => void; return 98765 as unknown as ReturnType<typeof window.setTimeout>; }
      return original(fn, delay, ...args) as unknown as ReturnType<typeof window.setTimeout>;
    });
    server.use(http.get(`${API}/branding`, () => HttpResponse.json(brand({ validUntil: new Date(now + 4_000_000_000).toISOString() }))));
    const { client } = renderWithApp(<Probe />, { user: null });
    await waitFor(() => expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network'));
    expect(wake).toBeDefined(); actual += 2_147_483_647;
    act(() => { wake!(); });
    expect(screen.getByLabelText('brand')).toHaveTextContent('Example Network');
    expect(effectiveBranding(client.getQueryData(brandingKey(API)), actual).enabled).toBe(true);
    expect(delays).toEqual([2_147_483_647, 4_000_000_000 - 2_147_483_647]);
  });
  it('ignores old API-base responses after replacement and restores head on unmount', async () => {
    let finish!: () => void;
    const wait = new Promise<void>((r) => { finish = r; });
    server.use(http.get(`${API}/branding`, async () => { await wait; return HttpResponse.json(brand()); }));
    const { unmount } = renderWithApp(<Probe />, { user: null });
    unmount(); await act(async () => { finish(); await Promise.resolve(); });
    expect(document.title).not.toBe('Example Network');
  });
});
