// / (signed in): the start page is the most useful list the person may
// see, the licenses for license admins and their own licenses for
// customers. Someone with neither (no role) is told why the portal is empty.
import { Text } from '@astryxdesign/core/Text';
import { useEffect } from 'react';
import { useCan } from '../api/context';
import { Page, PageHeader } from '../ui/components/layout/Page';
import { useNavigate } from '../ui/lib/router';

export default function HomePage() {
  const can = useCan();
  const navigate = useNavigate();
  const target = can('licenses.read') ? '/licenses' : can('self.read') ? '/my/licenses' : null;
  useEffect(() => {
    if (target) navigate(target, { replace: true });
  }, [target, navigate]);
  if (target) return null;
  return (
    <Page>
      <PageHeader title="No access yet" />
      <Text as="p">Your account holds no role in the license portal. Ask a superadmin to grant one.</Text>
    </Page>
  );
}
