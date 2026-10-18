// /invites/:token (signed in): an invite to a customer's licenses. The page
// names the customer first (POST /api/v1/me/invites/preview, which changes
// nothing), then links the invite to the signed-in account on Accept
// (POST /api/v1/me/invites/redeem). Any signed-in person may accept, so
// the page says which address the invite was sent to and which account
// accepts it.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { ApiError } from '../api/client';
import { useMe } from '../api/context';
import { useInvitePreview, useRouteMutation } from '../api/hooks';
import type { PageProps } from '../routes';
import { utcDay } from '../lib/dates';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';

export default function InvitePage({ params }: PageProps) {
  const token = params.token ?? '';
  const me = useMe();
  const preview = useInvitePreview(token);
  const redeem = useRouteMutation('myInviteRedeem');

  if (redeem.data) {
    return (
      <Page maxWidth={760}>
        <PageHeader title="Invite accepted" />
        <Banner status="success" title={`You now have access to the licenses of ${redeem.data.customer.name}`} description="Their licenses and keys are on your licenses page." />
        <HStack gap={2}>
          <Button label="Go to my licenses" variant="primary" href="/my/licenses" />
        </HStack>
      </Page>
    );
  }

  let body;
  if (preview.isPending) body = <Loading />;
  else if (preview.error) {
    body =
      preview.error instanceof ApiError && preview.error.key === 'invalid_token' ? (
        <Banner status="error" title="This invite link does not work" description="It was already used, or it is not a link the portal made. Ask whoever invited you for a new link." />
      ) : (
        <ErrorBanner error={preview.error} />
      );
  } else {
    const p = preview.data;
    body = p.expired ? (
      <Banner status="warning" title="This invite has expired" description={`It stopped working on ${utcDay(p.expiresAt)}. Ask ${p.customer.name} or the licensor for a new link.`} />
    ) : (
      <Panel title={`Accept access to the licenses of ${p.customer.name}?`}>
        <Text as="p">
          The invite was sent to {p.email}. Accepting links it to the account you are signed in with ({me.user.email ?? me.user.name}): you will see the licenses and keys of {p.customer.name}.
        </Text>
        <Text type="supporting" as="p">
          The link works until {utcDay(p.expiresAt)} and only once.
        </Text>
        {redeem.error ? <ErrorBanner error={redeem.error} title="The invite was not accepted" /> : null}
        <HStack gap={2}>
          <Button label="Accept" variant="primary" isLoading={redeem.isPending} onClick={() => redeem.mutate({ body: { token } })} />
          <Button label="Not now" variant="ghost" href="/" />
        </HStack>
      </Panel>
    );
  }
  return (
    <Page maxWidth={760}>
      <PageHeader title="Invite" />
      {body}
    </Page>
  );
}
