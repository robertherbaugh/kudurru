// A fixture API for looking at the pages in `vite dev` without a server
// (screenshots, layout work): `?mock` or `?mock=customer|admin|super|none`
// (none: signed out) at any address replaces window.fetch with the
// stand-in of ./api.ts answering from ./fixtures.ts. Approval is required
// (the default): requests and renewals answer pending_review, and
// /approve?token=<anything> previews the open fixture request (the token
// "used" shows a link that was already used). main.tsx loads this module
// only when import.meta.env.DEV is true, so production builds do not
// contain it.
import type { CustomerDetail, LicenseDetail } from '@shared/api';
import { apiError, createFetchMock, reply } from './api';
import * as f from './fixtures';

const KEY = 'kudurru-license-portal.mock';

export function startDevMock(): void {
  const asked = new URLSearchParams(window.location.search).get('mock');
  if (asked) window.sessionStorage.setItem(KEY, asked);
  const who = asked || window.sessionStorage.getItem(KEY) || 'admin';
  let signedIn = who !== 'none';
  const me = who === 'customer' ? f.customerMe : who === 'super' ? f.superMe : f.adminMe;
  const detail = (id: string): LicenseDetail => (id === 'lic_withdrawn' ? { ...f.withdrawnLicenseDetail, id } : { ...f.licenseDetail, id });
  const customer: CustomerDetail = f.customerDetail;
  const mock = createFetchMock({
    authConfig: () => ({ displayName: 'Sign in with Dex', devMode: true }),
    authMe: () => (signedIn ? me : apiError(401, 'unauthenticated', 'Sign in first')),
    authLogout: () => {
      signedIn = false;
      return reply(204);
    },
    catalog: () => f.catalog({ signing: { available: true, kid: 'dev-1', testKey: true } }),
    myLicenses: () => ({ items: [f.myLicense, { ...f.myLicense, id: 'lic_withdrawn', state: 'withdrawn' }] }),
    myLicense: ({ params }) => (params.id === 'lic_withdrawn' ? { ...f.myWithdrawnDetail, id: 'lic_withdrawn' } : { ...f.myLicenseDetail, renewal: { expiresOn: '2028-10-11' } }),
    myRequests: () => ({ items: [] }),
    myRequestCreate: () => ({ outcome: 'pending_review', requestId: 'req_mine' }),
    myLicenseRenew: () => ({ outcome: 'pending_review', requestId: 'req_renew' }),
    myInvitePreview: () => ({ customer: { id: customer.id, name: customer.name }, email: 'ada@example.org', expiresAt: '2026-10-26T09:00:00.000Z', expired: false }),
    myInviteRedeem: () => ({ customer: { id: customer.id, name: customer.name } }),
    publicKeyRequestVerify: () => ({ outcome: 'pending_review' }),
    approvalPreview: ({ body }) => ((body as { token?: string }).token === 'used' ? { ...f.approvalPreview, expired: true } : f.approvalPreview),
    approvalDecide: ({ body }) => ((body as { decision?: string }).decision === 'reject' ? { status: 'rejected' } : { status: 'approved', licenseId: 'lic_personal01', outcome: 'issued' }),
    licensesList: () => ({
      items: [
        f.licenseSummary,
        { ...f.licenseSummary, id: 'lic_personal01', licensee: 'Ada Lovelace', plan: 'personal', nodes: 3, features: null, state: 'expiring', expiresOn: '2026-11-01' },
        { ...f.licenseSummary, id: 'lic_withdrawn', licensee: 'Initech', state: 'withdrawn' },
      ],
      total: 3,
    }),
    licenseGet: ({ params }) => detail(params.id ?? ''),
    licenseIssue: () => f.licenseDetail,
    licenseRenew: () => f.licenseDetail,
    licenseChange: () => f.licenseDetail,
    licenseWithdraw: () => f.withdrawnLicenseDetail,
    licenseEmail: () => ({ sentTo: customer.contactEmail ?? 'someone@example.org' }),
    licenseImport: () => f.licenseDetail,
    customersList: () => ({ items: [customer], total: 1 }),
    customerGet: () => customer,
    customerCreate: () => customer,
    customerUpdate: () => customer,
    memberAdd: ({ body }) => ({ member: { ...customer.members[0], email: (body as { email: string }).email, status: 'invited', user: null }, inviteUrl: 'http://localhost:5174/invites/abcdefghijklmnop' }),
    memberInvite: () => ({ member: customer.members[0], inviteUrl: 'http://localhost:5174/invites/qrstuvwxyz' }),
    memberRemove: () => reply(204),
    requestsList: () => ({ items: [f.openRequest, f.renewalRequest] }),
    requestApprove: () => ({ request: { ...f.openRequest, status: 'approved' }, license: f.licenseDetail }),
    requestReject: () => ({ request: { ...f.openRequest, status: 'rejected' } }),
    businessRequestsList: ({ query }) => ({ items: [f.openBusinessRequest, f.issuedBusinessRequest].filter((r) => !query.get('status') || r.status === query.get('status')) }),
    businessRequestClose: ({ body }) => ({ request: { ...f.openBusinessRequest, status: 'closed', decisionNote: (body as { note: string }).note, decidedAt: '2026-10-13T09:00:00.000Z' } }),
    blockedEmailsList: () => ({ items: [{ email: 'mallory@example.org', reason: 'License withdrawn', blockedBy: { id: 'usr_admin', label: 'Alan Admin' }, blockedAt: '2026-09-01T10:00:00.000Z' }] }),
    blockedEmailRemove: () => reply(204),
    usersList: () => ({ items: [f.userDetail, { ...f.userDetail, id: 'usr_admin', name: 'Alan Admin', email: 'admin@example.org', idpRoles: ['license_admin'], roles: ['customer', 'license_admin'] }] }),
    userGet: () => f.userDetail,
    userUpdate: () => f.userDetail,
    userRevokeSessions: () => ({ revoked: 1 }),
    userDelete: () => reply(204),
    auditList: () => ({ items: f.auditEvents }),
    system: () => f.systemInfo,
  });
  window.fetch = mock.fetch;
}
