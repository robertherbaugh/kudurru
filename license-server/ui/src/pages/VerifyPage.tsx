// /verify?token= (anonymous, outside the session gate): the page the
// confirmation mail of the kudurru.io form links to. It says what happens
// next and confirms only on a click (mail scanners that open links must
// not consume the token), then shows the outcome: a key was mailed, the
// personal key was renewed or resent, or the request waits for review.
// The key itself only ever goes by mail.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMutation } from '@tanstack/react-query';
import { BUSINESS_CONTACT, CONFIRM_TTL_HOURS, KEY_IS_NOT_A_LICENCE, LICENSE_PAGE_URL, OFFLINE_NOTE } from '@shared/catalog';
import { ApiError } from '../api/client';
import { useApi } from '../api/context';
import { outcomeWords, PRODUCT_NAME } from '../lib/words';
import { BrandLogo } from '../ui/components/BrandLogo';
import { errorText } from '../ui/components/Feedback';
import { useLocation } from '../ui/lib/router';

/** Why a confirmation failed, in words. */
export function verifyErrorWords(err: unknown): { title: string; description: string } {
  if (err instanceof ApiError) {
    switch (err.key) {
      case 'invalid_token':
      case 'invalid':
        return {
          title: 'This confirmation link no longer works',
          description: `It was already used, or it is older than ${CONFIRM_TTL_HOURS} hours. If you already confirmed, check your mail for the key; otherwise request a key again.`,
        };
      case 'rate_limited':
        return { title: 'Too many attempts', description: `Wait a little and try again${err.retryAfter ? ` in ${err.retryAfter} seconds` : ''}.` };
      case 'unavailable':
      case 'signing_unavailable':
        return { title: 'Keys cannot be sent right now', description: `Try again later, or write to ${BUSINESS_CONTACT}.` };
      default:
        break;
    }
  }
  return { title: 'The request could not be confirmed', description: errorText(err) };
}

export default function VerifyPage() {
  const client = useApi();
  const loc = useLocation();
  const token = loc.search.get('token') ?? '';
  const confirm = useMutation({ mutationFn: () => client.call('publicKeyRequestVerify', { body: { token } }) });

  let body;
  if (!token) {
    body = <Banner status="warning" title="This page needs the link from your mail" description="Open the confirmation link in the mail we sent you after you requested a free personal key." />;
  } else if (confirm.data) {
    const w = outcomeWords(confirm.data.outcome, null, 'web');
    body = (
      <VStack gap={3}>
        <Banner status={confirm.data.outcome === 'pending_review' ? 'info' : 'success'} title={w.title} description={w.body} />
        <Text as="p" type="supporting">
          {KEY_IS_NOT_A_LICENCE}
        </Text>
      </VStack>
    );
  } else {
    const failed = confirm.error ? verifyErrorWords(confirm.error) : null;
    body = (
      <VStack gap={3}>
        <Text as="p">
          You asked for a free personal key for non-commercial use. Confirm the request and we mail the key to the address you gave. If you already hold a personal key, we
          renew it when it is due or mail it again.
        </Text>
        {failed ? (
          <Banner
            status="error"
            title={failed.title}
            description={failed.description}
            endContent={
              <Link href={LICENSE_PAGE_URL} isExternalLink>
                Request a key
              </Link>
            }
          />
        ) : null}
        <Button label="Confirm and send my key" variant="primary" width="100%" isLoading={confirm.isPending} isDisabled={confirm.error instanceof ApiError && (confirm.error.key === 'invalid_token' || confirm.error.key === 'invalid')} onClick={() => confirm.mutate()} />
      </VStack>
    );
  }

  return (
    <Center role="main" minHeight="100dvh" padding={4}>
      <Card width="100%" maxWidth={520} padding={6}>
        <VStack gap={4}>
          <VStack gap={2}>
            <BrandLogo tagline alt="kudurru: authority over every zone." />
            <Heading level={1}>Confirm your key request</Heading>
            <Text as="p" type="supporting">
              {PRODUCT_NAME}. {OFFLINE_NOTE}
            </Text>
          </VStack>
          {body}
        </VStack>
      </Card>
    </Center>
  );
}
