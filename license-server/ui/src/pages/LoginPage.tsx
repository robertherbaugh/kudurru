// /login (anonymous): the product name, the offline note, one sign-in
// button that starts the OIDC flow on the server (GET /api/v1/auth/login
// with next=, a full page load), the reason a sign-in failed (?error= from
// the callback, a short fixed code) and the way to a free personal key
// without an account. A visitor who is already signed in goes on to next.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useEffect } from 'react';
import { LICENSE_PAGE_URL, OFFLINE_NOTE } from '@shared/catalog';
import { useApi, useSession } from '../api/context';
import { useAuthConfig } from '../api/hooks';
import { PRODUCT_NAME } from '../lib/words';
import { BrandLogo } from '../ui/components/BrandLogo';
import { FullPageSpinner } from '../ui/components/Feedback';
import { useLocation, useNavigate } from '../ui/lib/router';

/**
 * The reasons /login?error= carries, as server/auth/loginErrors.ts
 * LOGIN_ERROR_KEYS lists them. The UI cannot import server code; the
 * frozen change request moves the list to shared/api.ts, and then this
 * copy goes.
 */
export const LOGIN_ERROR_KEYS = ['idp_error', 'expired', 'failed', 'idp_unavailable', 'disabled', 'no_role', 'rate_limited', 'login_busy'] as const;
export type LoginErrorKey = (typeof LOGIN_ERROR_KEYS)[number];

function isLoginErrorKey(v: string): v is LoginErrorKey {
  return (LOGIN_ERROR_KEYS as readonly string[]).includes(v);
}

/** The words for one reason (exhaustive: a new key fails the type check here). */
function loginErrorWords(key: LoginErrorKey): { title: string; description: string } {
  switch (key) {
    case 'idp_error':
      return { title: 'The identity provider did not sign you in', description: 'The sign-in was canceled or refused there. Try again; if it keeps failing, ask the administrator of your identity provider.' };
    case 'expired':
      return {
        title: 'The sign-in took too long or was started in another browser',
        description: 'A sign-in has to finish within 10 minutes, in the browser that started it. Start it again here.',
      };
    case 'failed':
      return { title: 'The sign-in could not be checked', description: "The portal could not confirm the identity provider's answer. Try again; if it keeps failing, tell the portal's operator." };
    case 'idp_unavailable':
      return { title: 'The identity provider cannot be reached', description: 'The portal could not contact it. Try again in a few minutes.' };
    case 'disabled':
      return { title: 'This account is disabled', description: 'A superadmin of the license portal disabled it. Ask them to enable it again.' };
    case 'no_role':
      return { title: 'Your account has no access to the license portal', description: 'Your identity provider maps you to no role here. Ask a superadmin to grant one.' };
    case 'rate_limited':
      return { title: 'Too many sign-in attempts', description: 'Too many sign-ins came from your network. Wait a minute and try again.' };
    case 'login_busy':
      return { title: 'The portal is busy', description: 'Too many sign-ins are in progress right now. Try again in a minute.' };
  }
}

/** The words for the callback's error codes (spec §6: /login?error=<short fixed message>). */
export function signInError(code: string): { title: string; description: string } {
  if (isLoginErrorKey(code)) return loginErrorWords(code);
  // Only the server's fixed codes are expected; anything else is shown as plain, clipped text.
  const text = code.replace(/[^\x20-\x7e]/g, '').slice(0, 120);
  return { title: 'Sign-in did not complete', description: text ? `The sign-in failed (${text}). Try again.` : 'Try again.' };
}

/** The `next` of the sign-in: an app route ("/…", never "//…"), else "/". The server checks it again (SafeNext). */
export function safeNext(raw: string | null): string {
  if (!raw || !raw.startsWith('/') || raw.startsWith('//') || raw.startsWith('/\\') || raw.startsWith('/login')) return '/';
  return raw;
}

export default function LoginPage() {
  const client = useApi();
  const loc = useLocation();
  const navigate = useNavigate();
  const session = useSession();
  const config = useAuthConfig();
  const next = safeNext(loc.search.get('next'));
  const error = loc.search.get('error');
  const signedIn = Boolean(session.data);

  useEffect(() => {
    if (signedIn) navigate(next, { replace: true });
  }, [signedIn, next, navigate]);

  if (session.isPending || signedIn) return <FullPageSpinner label="Connecting to the license portal" />;
  const failed = error ? signInError(error) : null;
  return (
    <Center role="main" minHeight="100dvh" padding={4}>
      <Card width="100%" maxWidth={460} padding={6}>
        <VStack gap={4}>
          <VStack gap={2}>
            <BrandLogo tagline alt="kudurru: authority over every zone." />
            <Heading level={1}>{PRODUCT_NAME}</Heading>
            <Text as="p" type="supporting">
              Licenses and keys for kudurru. {OFFLINE_NOTE}
            </Text>
          </VStack>
          {failed ? <Banner status="error" title={failed.title} description={failed.description} /> : null}
          {config.data?.devMode ? <Banner status="warning" title="Development mode" description="This portal signs keys with a test key that no kudurru release trusts." /> : null}
          <Button label={config.data?.displayName || 'Sign in'} variant="primary" width="100%" href={client.href('authLogin', {}, { next })} />
          <Text as="p" type="supporting">
            <Link href={LICENSE_PAGE_URL} isExternalLink>
              Request a free personal key without an account
            </Link>
          </Text>
        </VStack>
      </Card>
    </Center>
  );
}
