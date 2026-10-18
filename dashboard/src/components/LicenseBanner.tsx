// The license notice above every page: a key that is invalid, expired, in
// its grace period or close to expiring, or a cluster over its node limit.
// A node without a key is a supported way to run and shows nothing here.
// Errors and warnings stay until fixed; the expiry reminder can be closed
// for the browser session.
import { Banner } from '@astryxdesign/core/Banner';
import { Link } from '@astryxdesign/core/Link';
import { useState } from 'react';
import { useLicense } from '../api/hooks';
import { licenseNotice } from '../lib/license';

const KEY = 'kudurru.licenseNoticeDismissed';

export function LicenseBanner() {
  const q = useLicense();
  const [dismissed, setDismissed] = useState(() => {
    try {
      return window.sessionStorage.getItem(KEY) === '1';
    } catch {
      return false;
    }
  });
  const notice = licenseNotice(q.data);
  if (!notice) return null;
  const closable = notice.status === 'info';
  if (closable && dismissed) return null;
  return (
    <Banner
      data-testid="license-banner"
      status={notice.status}
      container="section"
      title={notice.title}
      description={notice.description}
      endContent={<Link href="/license">View license</Link>}
      isDismissable={closable}
      onDismiss={() => {
        setDismissed(true);
        try {
          window.sessionStorage.setItem(KEY, '1');
        } catch {
          // ignore
        }
      }}
    />
  );
}
