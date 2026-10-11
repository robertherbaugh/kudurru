// The "default credentials" nag: shown while GET /auth/status reports
// hasDefaultCredentials (the bootstrap admin still has its first-boot
// password), dismissable per browser session.
import { Banner } from '@astryxdesign/core/Banner';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../api/context';

const KEY = 'kudurru.defaultCredsDismissed';

export function DefaultCredentialsBanner() {
  const api = useApi();
  const q = useQuery({ queryKey: qk.authStatus, queryFn: () => api.auth.status(), staleTime: 60_000 });
  const [dismissed, setDismissed] = useState(() => {
    try {
      return window.sessionStorage.getItem(KEY) === '1';
    } catch {
      return false;
    }
  });
  if (!q.data?.hasDefaultCredentials || dismissed) return null;
  return (
    <Banner
      status="warning"
      container="section"
      title="The admin account still uses its default password"
      description="Change it with `kudurru admin reset-password` on the host, or sign in through an identity provider and disable local login."
      isDismissable
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
