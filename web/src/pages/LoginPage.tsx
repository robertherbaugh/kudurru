// Sign-in (§8.2): the local username/password form when local login is
// enabled, one button per OIDC provider from GET /auth/status (the BFF
// flow starts at /auth/oidc/{idp}/start and comes back with the session
// cookie), and the default-credentials nag.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useApiContext } from '../api/context';
import { BrandLogo } from '../components/BrandLogo';
import { errorText } from '../components/Feedback';
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

  const localEnabled = status.data ? status.data.localLogin !== false && status.data.localLogin !== 'disabled' : true;
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
    <main className="k-login">
      <Card className="k-login-card">
        <div className="k-stack-lg">
          <div className="k-stack">
            <BrandLogo tagline alt="kudurru: authority over every zone." />
            <h1 className="k-login-title">Sign in</h1>
          </div>
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
            <form
              className="k-form"
              onSubmit={(e) => {
                e.preventDefault();
                void submit();
              }}
            >
              <TextInput label="Username" value={username} onChange={setUsername} autoComplete="username" hasAutoFocus isRequired />
              <TextInput
                label="Password"
                type="password"
                value={password}
                onChange={setPassword}
                autoComplete="current-password"
                isRequired
              />
              {error ? (
                <p className="k-error-text" role="alert">
                  {error}
                </p>
              ) : null}
              <Button type="submit" label="Sign in" variant="primary" isLoading={busy} width="100%" />
            </form>
          ) : (
            <p className="k-muted">Local sign-in is disabled on this server.</p>
          )}
          {providers.length > 0 ? (
            <div className="k-stack" role="group" aria-label="Single sign-on">
              {localEnabled ? <p className="k-divider-text">or continue with</p> : null}
              {providers.map((p) => (
                <Button
                  key={p.id}
                  label={p.displayName}
                  variant="secondary"
                  width="100%"
                  href={api.auth.oidcStartURL(p.id, loc.route === '/login' ? '/' : loc.route)}
                />
              ))}
            </div>
          ) : null}
        </div>
      </Card>
    </main>
  );
}

export default LoginPage;
