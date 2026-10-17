// The Settings row extra of blocking.catalogue.adguardServicesURL (whose
// help explains the opt-in and its GPL-3.0 licence): the state of the
// import from GET /catalogue (sources), nothing while it is off.
import { Banner } from '@astryxdesign/core/Banner';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useApi, useCan } from '../api/context';
import { formatRelative } from '../lib/format';
import { catalogueInfoKey } from './ServiceGrid';

export function AdGuardImportNote() {
  const api = useApi();
  const can = useCan();
  const info = useQuery({ queryKey: catalogueInfoKey, queryFn: () => api.catalogue.info(), enabled: can('policies', 'read'), staleTime: 60_000 });
  const src = info.data?.sources?.find((s) => s.id === 'adguard');
  return (
    <VStack gap={2} data-testid="adguard-import-note">
      {src ? (
        src.loaded ? (
          <Text type="supporting" display="block">
            In use: {src.services} services from {src.origin}
            {src.shadowed ? `, ${src.shadowed} left out because kudurru has them` : ''}
            {src.refresh?.lastSuccess ? `; checked ${formatRelative(src.refresh.lastSuccess)}` : ''}.
          </Text>
        ) : (
          <Banner status="warning" title="Not downloaded yet" description={src.refresh?.lastError ? `The last attempt failed: ${src.refresh.lastError}` : 'The import starts after the restart.'} />
        )
      ) : null}
      {src?.loaded && src.refresh?.lastError ? <Banner status="warning" title="The last refresh failed; the previous download stays in use" description={src.refresh.lastError} /> : null}
    </VStack>
  );
}
