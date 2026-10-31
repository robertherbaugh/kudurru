// Public presentation only. Authentication never depends on branding availability.
import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useApiContext } from '../api/context';
import type { BrandingInfo } from '../api/types';
import { brandingAccentValid, brandingNameValid, brandingPNGValid } from './brandingAssets';

export const DEFAULT_BRANDING: BrandingInfo = Object.freeze({ enabled: false, name: 'kudurru', logoLight: '', logoDark: '', favicon: '', accentColor: '' });
export const brandingKey = (base: string) => ['branding', base] as const;

export function effectiveBranding(value: unknown, now = Date.now()): BrandingInfo {
  if (!value || typeof value !== 'object') return DEFAULT_BRANDING;
  const b = value as BrandingInfo;
  if (b.enabled !== true || typeof b.validUntil !== 'string' || !/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)$/.test(b.validUntil) || !Number.isFinite(Date.parse(b.validUntil)) || Date.parse(b.validUntil) <= now) return DEFAULT_BRANDING;
  if (typeof b.name !== 'string' || !brandingNameValid(b.name) || typeof b.accentColor !== 'string' || !brandingAccentValid(b.accentColor)) return DEFAULT_BRANDING;
  if (typeof b.logoLight !== 'string' || typeof b.logoDark !== 'string' || typeof b.favicon !== 'string' || !brandingPNGValid(b.logoLight) || !brandingPNGValid(b.logoDark) || !brandingPNGValid(b.favicon, 256)) return DEFAULT_BRANDING;
  return b;
}

const BrandingContext = createContext<BrandingInfo>(DEFAULT_BRANDING);
export function useBranding(): BrandingInfo { return useContext(BrandingContext); }

export function BrandingProvider({ children }: { children: ReactNode }) {
  const { api, client } = useApiContext();
  const qc = useQueryClient();
  const [, tick] = useState(0);
  const query = useQuery({
    queryKey: brandingKey(client.baseUrl), queryFn: async ({ signal }) => {
      const value = await api.branding.get(signal);
      signal.throwIfAborted();
      return effectiveBranding(value);
    },
    staleTime: 0, retry: false, refetchInterval: 15_000, refetchIntervalInBackground: true, refetchOnWindowFocus: 'always',
  });
  const brand = query.isError ? DEFAULT_BRANDING : effectiveBranding(query.data);
  useEffect(() => {
    const recheck = () => tick((n) => n + 1);
    window.addEventListener('focus', recheck);
    document.addEventListener('visibilitychange', recheck);
    const deadline = query.data?.enabled ? Date.parse(query.data.validUntil!) : NaN;
    let timer: number | undefined;
    const wake = () => {
      if (Date.now() < deadline) {
        timer = window.setTimeout(wake, Math.min(2_147_483_647, Math.max(1, deadline - Date.now())));
        return;
      }
      // Replace only this response. A late timer cannot erase a newer capture.
      if (qc.getQueryData(brandingKey(client.baseUrl)) === query.data) qc.setQueryData(brandingKey(client.baseUrl), DEFAULT_BRANDING);
      recheck();
      void qc.invalidateQueries({ queryKey: brandingKey(client.baseUrl) });
    };
    if (Number.isFinite(deadline)) timer = window.setTimeout(wake, Math.min(2_147_483_647, Math.max(0, deadline - Date.now())));
    return () => { window.clearTimeout(timer); window.removeEventListener('focus', recheck); document.removeEventListener('visibilitychange', recheck); };
  }, [brand.enabled, brand.validUntil, client.baseUrl, qc, query.data]);
  useEffect(() => {
    if (!brand.enabled) return;
    const title = document.title;
    document.title = brand.name || 'kudurru';
    const originals = brand.favicon ? Array.from(document.head.querySelectorAll<HTMLLinkElement>('link[rel="icon"],link[rel="shortcut icon"]')) : [];
    // Retain the exact original nodes, attributes, and positions for restoration.
    const slots = originals.map((node) => { const marker = document.createComment('branding icon'); node.replaceWith(marker); return { node, marker }; });
    const icon = brand.favicon ? document.createElement('link') : null;
    if (icon) { icon.rel = 'icon'; icon.type = 'image/png'; icon.href = brand.favicon; document.head.append(icon); }
    return () => { document.title = title; icon?.remove(); for (const { node, marker } of slots) marker.replaceWith(node); };
  }, [brand.enabled, brand.name, brand.favicon]);
  return <BrandingContext.Provider value={brand}>{children}</BrandingContext.Provider>;
}
