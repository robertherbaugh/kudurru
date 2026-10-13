// Copied from web/src/components/BrandLogo.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// The kudurru logo from docs/brand: the Clay tablet with the Ink wordmark
// on light surfaces and the Paper wordmark on dark ones (styles.css picks
// one by the resolved color scheme). The images are downscaled copies of
// the docs/brand exports (web/scripts/brand-assets.py).
import logoInk from '../assets/kudurru-logo-mixed.png';
import logoPaper from '../assets/kudurru-logo-mixed-paper.png';
import taglineInk from '../assets/kudurru-logo-mixed-tagline.png';
import taglinePaper from '../assets/kudurru-logo-mixed-paper-tagline.png';

/**
 * The header logo (180 x 60) or, with `tagline`, the sign-in logo with
 * "authority over every zone." (360 x 150). `alt` follows the brand
 * README: "kudurru" when the logo stands alone, empty next to a visible
 * name.
 */
export function BrandLogo({ tagline, alt = 'kudurru' }: { tagline?: boolean; alt?: string }) {
  const [light, dark, w, h, cls] = tagline ? [taglineInk, taglinePaper, 360, 150, 'k-logo-tagline'] : [logoInk, logoPaper, 180, 60, 'k-logo'];
  return (
    <>
      <img className={`${cls} k-logo-on-light`} src={light} width={w} height={h} alt={alt} />
      <img className={`${cls} k-logo-on-dark`} src={dark} width={w} height={h} alt={alt} />
    </>
  );
}
