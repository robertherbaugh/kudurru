// Sign-in (§8.2): the local username/password form when local login is
// enabled, one button per OIDC provider from GET /auth/status (the BFF
// flow starts at /auth/oidc/{idp}/start and comes back with the session
// cookie), and the default-credentials nag.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { Divider } from '@astryxdesign/core/Divider';
import { Heading } from '@astryxdesign/core/Heading';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useApiContext } from '../api/context';
import { BrandLogo } from '../components/BrandLogo';
import { errorText } from '../components/Feedback';
import { TextField } from '../components/fields/TextField';
import { useLocation } from '../lib/router';

export function LoginPage() {
  const api = useApi();
  const { setCsrf } = useApiContext();
  const qc = useQueryClient();
  const loc = useLocation();
  const status = useQuery({ queryKey: qk.authStatus, queryFn: () => api.auth.status(), retry: 1 });
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();
  const oidcError = loc.search.get('error');

  const localEnabled = status.data ? status.data.localLogin && status.data.localLoginMode !== 'disabled' : true;
  const providers = status.data?.providers ?? [];

  const submit = async () => {
    if (!username || !password) {
      setError('Enter a username and password');
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const me = await api.auth.login(username, password);
      setCsrf(me.csrf);
      qc.setQueryData(qk.me, me);
      await qc.invalidateQueries({ queryKey: qk.authStatus });
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Center role="main" minHeight="100dvh" padding={4}>
      <Card width="100%" maxWidth={420} padding={6}>
        <VStack gap={4}>
          <VStack gap={2}>
            <BrandLogo tagline alt="kudurru: authority over every zone." />
            <Heading level={1}>Sign in</Heading>
          </VStack>
          {status.data?.hasDefaultCredentials ? (
            <Banner
              status="warning"
              title="Default credentials are active"
              description="The bootstrap admin still has its first-boot password. Sign in and change it, or run `kudurru admin reset-password` on the host."
            />
          ) : null}
          {oidcError ? <Banner status="error" title="Single sign-on failed" description={oidcError} /> : null}
          {status.isError ? (
            <Banner
              status="error"
              title={status.error instanceof ApiError && status.error.status === 0 ? 'The kudurru API is unreachable' : 'Sign-in options could not be loaded'}
              description={errorText(status.error)}
            />
          ) : null}
          {localEnabled ? (
            <VStack
              as="form"
              gap={3}
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              <TextField label="Username" value={username} onChange={setUsername} autoComplete="username" hasAutoFocus isRequired />
              <TextField
                label="Password"
                type="password"
                value={password}
                onChange={setPassword}
                autoComplete="current-password"
                isRequired
              />
              {/* An error Banner is role="alert": announced, and what the e2e looks for. */}
              {error ? <Banner status="error" title={error} /> : null}
              <Button type="submit" label="Sign in" variant="primary" isLoading={busy} width="100%" />
            </VStack>
          ) : (
            <Text color="secondary" as="p">
              Local sign-in is disabled on this server.
            </Text>
          )}
          {providers.length > 0 ? (
            <VStack gap={2} role="group" aria-label="Single sign-on">
              {localEnabled ? <Divider label="or continue with" /> : null}
              {providers.map((p) => (
                <Button
                  key={p.id}
                  label={p.displayName}
                  variant="secondary"
                  width="100%"
                  href={api.auth.oidcStartURL(p.id, loc.route === '/login' ? '/' : loc.route)}
                />
              ))}
            </VStack>
          ) : null}
        </VStack>
      </Card>
    </Center>
  );
}

export default LoginPage;
