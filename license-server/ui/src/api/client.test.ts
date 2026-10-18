// The API client: paths come from the route manifest under the base, the
// CSRF header rides on mutating requests only, errors keep the contract's
// key, message, hint and fields, a 401 outside /auth/* clears the session,
// a network failure says the portal could not be reached, and a 400's
// fields reach the controls they name (fieldErrors.ts).
import { ApiClient, ApiError, UNREACHABLE } from './client';
import { apiError, createFetchMock, reply } from '../dev/api';
import { bannerError, fieldError, firstField, NOT_VALID, SEE_BELOW } from './fieldErrors';
import { utcDateTime, utcDay } from '../lib/dates';

function client(handlers: Parameters<typeof createFetchMock>[0], onUnauthenticated = vi.fn()) {
  const mock = createFetchMock(handlers);
  const c = new ApiClient({ baseUrl: '/portal/api/v1', csrf: () => 'tok', onUnauthenticated, fetch: mock.fetch });
  return { c, mock, onUnauthenticated };
}

test('builds paths from the manifest under the base and sends CSRF on mutations only', async () => {
  const { c, mock } = client({ licenseGet: () => ({ id: 'x' }), licenseRenew: () => ({ id: 'x' }) });
  await c.call('licenseGet', { params: { id: 'lic/1' } });
  await c.call('licenseRenew', { params: { id: 'lic_1' }, body: { expiresOn: '2028-01-01' } });
  const [get, post] = mock.calls;
  expect(get?.path).toBe('/api/v1/licenses/lic%2F1');
  expect(get?.headers.get('X-Kudurru-CSRF')).toBeNull();
  expect(post?.headers.get('X-Kudurru-CSRF')).toBe('tok');
  expect(post?.headers.get('Content-Type')).toBe('application/json');
  expect(post?.body).toEqual({ expiresOn: '2028-01-01' });
  expect(c.href('myLicenseKey', { id: 'lic_1' })).toBe('/portal/api/v1/me/licenses/lic_1/key');
  expect(c.href('authLogin', {}, { next: '/invites/a b' })).toBe('/portal/api/v1/auth/login?next=%2Finvites%2Fa+b');
});

test('errors keep the envelope; 401 clears the session except on /auth/*', async () => {
  const { c, onUnauthenticated } = client({
    licenseIssue: () => apiError(400, 'invalid', 'Check the fields', { fields: ['newCustomer.name', 'features'] }),
    licensesList: () => apiError(401, 'unauthenticated', 'Sign in'),
    authMe: () => apiError(401, 'unauthenticated', 'Sign in'),
    usersList: () => apiError(403, 'forbidden', 'Not permitted', { hint: { missing: 'users.read' } }),
    memberRemove: () => reply(204),
  });
  const invalid = await c.call('licenseIssue', { body: { customerId: 'c', licensee: 'x', plan: 'business', nodes: 1, features: null, expiresOn: '2027-01-01' } }).catch((e: unknown) => e);
  expect(invalid).toBeInstanceOf(ApiError);
  expect((invalid as ApiError).hasField('newCustomer')).toBe(true);
  expect((invalid as ApiError).fields).toEqual(['newCustomer.name', 'features']);
  const forbidden = (await c.call('usersList', {}).catch((e: unknown) => e)) as ApiError;
  expect(forbidden.missing).toBe('users.read');
  expect(forbidden.describe()).toBe('Not permitted (needs users.read)');
  await c.call('authMe').catch(() => undefined);
  expect(onUnauthenticated).not.toHaveBeenCalled();
  await c.call('licensesList', {}).catch(() => undefined);
  expect(onUnauthenticated).toHaveBeenCalledTimes(1);
  await expect(c.call('memberRemove', { params: { id: 'cus_1', email: 'a@b.c' } })).resolves.toBeUndefined();
});

test('a network failure is a status-0 ApiError', async () => {
  const c = new ApiClient({ baseUrl: '/api/v1', fetch: () => Promise.reject(new TypeError('Failed to fetch')) });
  const err = (await c.call('catalog').catch((e: unknown) => e)) as ApiError;
  expect(err.status).toBe(0);
  expect(err.message).toBe(UNREACHABLE);
});

test('dates are UTC days with the (UTC) label, whatever the instant', () => {
  expect(utcDay('2027-10-12')).toBe('2027-10-12 (UTC)');
  expect(utcDay('2026-10-12T23:30:00.000Z')).toBe('2026-10-12 (UTC)');
  expect(utcDay('2026-10-12T23:30:00-05:00')).toBe('2026-10-13 (UTC)');
  expect(utcDay(null)).toBe('–');
  expect(utcDateTime('2026-10-12T09:05:59.000Z')).toBe('2026-10-12 09:05 (UTC)');
});

test('describe: only 500 internal names a request id; other hints are details', () => {
  expect(new ApiError(500, 'internal', 'Internal error.', 'req_123').describe()).toBe('Internal error. (request req_123)');
  const signing = new ApiError(503, 'signing_unavailable', 'Keys cannot be signed right now: the portal has no usable signing key.', 'no signing key is configured');
  expect(signing.describe()).toBe('Keys cannot be signed right now: the portal has no usable signing key (no signing key is configured).');
  expect(new ApiError(503, 'unavailable', 'Try later', 'maintenance').describe()).toBe('Try later (maintenance)');
  expect(new ApiError(429, 'rate_limited', 'Slow down.', undefined, 30).describe()).toBe('Slow down. (try again in 30 s)');
});

test('field errors: a control shows its own message, the banner only what no control shows', () => {
  const one = new ApiError(400, 'invalid', 'A renewal must expire after the current expiry (2027-01-01 UTC).', undefined, undefined, ['expiresOn']);
  expect(fieldError(one, 'expiresOn')).toBe(one.message);
  expect(fieldError(one, 'emailCustomer')).toBeUndefined();
  expect(bannerError(one, ['expiresOn', 'emailCustomer'])).toBeNull();
  expect(bannerError(one, ['emailCustomer'])).toBe(one.message);

  // The body validator's message lists wire paths: never shown.
  const validator = new ApiError(400, 'invalid', 'Some values are missing or not valid: newCustomer.contactEmail, expiresOn.', undefined, undefined, ['newCustomer.contactEmail', 'expiresOn']);
  expect(fieldError(validator, 'newCustomer')).toBe(NOT_VALID);
  expect(fieldError(validator, 'newCustomer.contactEmail')).toBe(NOT_VALID);
  expect(bannerError(validator, ['newCustomer', 'expiresOn'])).toBeNull();
  expect(bannerError(validator, ['expiresOn'])).toBe('Some values are missing or not valid.');
  expect(firstField(validator, ['customerId', 'newCustomer.contactEmail', 'expiresOn'])).toBe('newCustomer.contactEmail');

  // Several fields with the policy's sentences: marked inline, the sentences in the banner.
  const policy = new ApiError(400, 'invalid', 'The licensee must be 1 to 200 characters. Nodes must be a whole number from 1 to 100000.', undefined, undefined, ['licensee', 'nodes']);
  expect(fieldError(policy, 'nodes')).toBe(SEE_BELOW);
  expect(bannerError(policy, ['licensee', 'nodes'])).toBe(policy.message);

  // Errors without fields stay in the banner.
  const conflict = new ApiError(409, 'conflict', 'This request is already approved.');
  expect(fieldError(conflict, 'expiresOn')).toBeUndefined();
  expect(bannerError(conflict, ['expiresOn'])).toBe('This request is already approved.');
  expect(bannerError(null, [])).toBeNull();
});
