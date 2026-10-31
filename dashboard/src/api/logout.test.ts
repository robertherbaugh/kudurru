import { describe, expect, it } from 'vitest';
import { browserLogoutContinuation } from './logout';

describe('browser logout continuation', () => {
  it('accepts only the same-origin exact API continuation, including a prefix', () => {
    expect(browserLogoutContinuation('/prefix/api/v1', '/prefix/api/v1/auth/oidc/corp/logout', 'https://app.example'))
      .toBe('/prefix/api/v1/auth/oidc/corp/logout');
    expect(browserLogoutContinuation('https://app.example/api/v1', '/api/v1/auth/oidc/corp/logout', 'https://app.example'))
      .toBe('/api/v1/auth/oidc/corp/logout');
  });
  it.each([
    undefined, '', '//evil.example/logout', 'https://evil.example/logout', '/api/v1/auth/oidc/corp/logout?next=https://evil.example',
    '/api/v1/auth/oidc/corp/logout#private', '/api/v1/auth/oidc/corp/callback', '/other/auth/oidc/corp/logout',
    '/api/v1/auth/oidc/corp%2flogout', '/api/v1/auth/oidc/corp\\logout', '/api/v1/auth/oidc/../logout',
    '/api/v1/auth/oidc/x/../corp/logout', '/api/v1/auth/oidc/corp/logout\n',
    '/api/v1/auth/oidc/CORP/logout', '/api/v1/auth/oidc/-corp/logout',
  ])('refuses an untrusted continuation', (path) => {
    expect(browserLogoutContinuation('/api/v1', path, 'https://app.example')).toBeUndefined();
  });
  it('rejects a foreign API origin', () => {
    expect(browserLogoutContinuation('https://foreign.example/api/v1', '/api/v1/auth/oidc/corp/logout', 'https://app.example')).toBeUndefined();
  });
});
