// The kudurru logo from branding: the Clay tablet with the Ink wordmark
// on light surfaces and the Paper wordmark on dark ones (styles.css picks
// one by the resolved colour scheme). The images are downscaled copies of
// the branding exports (dashboard/scripts/brand-assets.py).
import logoInk from '../assets/kudurru-logo-mixed.png';
import logoPaper from '../assets/kudurru-logo-mixed-paper.png';
import taglineInk from '../assets/kudurru-logo-mixed-tagline.png';
import taglinePaper from '../assets/kudurru-logo-mixed-paper-tagline.png';

import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import { useBranding } from '../lib/branding';

/** Safe captured logos; a single supplied image serves either mode. */
export function BrandLogo({ tagline, alt }: { tagline?: boolean; alt?: string }) {
  const brand = useBranding();
  const [failed, setFailed] = useState<string[]>([]);
  const customLight = failed.includes(brand.logoLight) ? '' : brand.logoLight;
  const customDark = failed.includes(brand.logoDark) ? '' : brand.logoDark;
  const custom = !!(customLight || customDark);
  if (!custom && brand.enabled && brand.name && brand.name !== 'kudurru') return <Text weight="semibold" size={tagline ? 'xl' : 'lg'} maxLines={tagline ? 2 : 1} className={tagline ? 'k-brand-name-signin' : 'k-brand-name'}>{brand.name}</Text>;
  const [light, dark, w, h, cls] = tagline ? [taglineInk, taglinePaper, 360, 150, 'k-logo-tagline'] : [logoInk, logoPaper, 180, 60, 'k-logo'];
  const name = alt ?? (brand.name || 'kudurru');
  return <>
    <img className={`${cls} k-logo-on-light${custom ? ' k-brand-custom' : ''}`} src={customLight || customDark || light} width={w} height={h} alt={name} onError={custom ? () => setFailed((v) => [...v, customLight || customDark].slice(-2)) : undefined} />
    <img className={`${cls} k-logo-on-dark${custom ? ' k-brand-custom' : ''}`} src={customDark || customLight || dark} width={w} height={h} alt={name} onError={custom ? () => setFailed((v) => [...v, customDark || customLight].slice(-2)) : undefined} />
  </>;
}
