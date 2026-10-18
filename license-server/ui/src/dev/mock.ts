// A fixture API for looking at the pages in `vite dev` without a server
// (screenshots, layout work): `?mock` or `?mock=customer|admin|super|none`
// (none: signed out) at any address replaces window.fetch with the
// stand-in of ./api.ts answering from ./fixtures.ts. Approval is required
// (the default): requests and renewals answer pending_review, and
// /approve?token=<anything> previews the open fixture request (the token
// "used" shows a link that was already used). main.tsx loads this module
// only when import.meta.env.DEV is true, so production builds do not
// contain it. Order Forms live in memory for the session: create, edit,
// send, revise, countersign, void, and the customer's accept and decline
// change them (no gap or hash is computed: sending sets the fixture hash).
import type { AcceptOrderFormBody, CreateOrderFormBody, CustomerDetail, DeclineOrderFormBody, LicenseDetail, MyOrderForm, OrderForm, OrderFormTerms } from '@shared/api';
import { orderFormGaps } from '@shared/orderForms';
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
  const now = () => new Date().toISOString();
  const orders = new Map<string, OrderForm>([f.draftOrderForm, f.sentOrderForm, f.acceptedOrderForm, f.executedOrderForm].map((o) => [o.id, o]));
  const order = (id: string | undefined) => orders.get(id ?? '');
  const save = (o: OrderForm, patch: Partial<OrderForm>) => {
    const terms = patch.terms ?? o.terms;
    const next: OrderForm = { ...o, ...patch, updatedAt: now(), gaps: orderFormGaps(terms).map((g) => `terms.${g}`) };
    orders.set(next.id, next);
    return next;
  };
  const act = (id: string | undefined, from: OrderForm['status'][], patch: (o: OrderForm) => Partial<OrderForm>) => {
    const o = order(id);
    if (!o) return apiError(404, 'not_found', 'No such Order Form');
    if (!from.includes(o.status)) return apiError(409, 'conflict', `The Order Form is ${o.status}`);
    return save(o, patch(o));
  };
  const mine = (o: OrderForm): MyOrderForm => ({
    id: o.id,
    number: o.number,
    status: o.status,
    version: o.version,
    customer: { id: o.customer.id, name: o.customer.name },
    terms: o.terms,
    documentSha256: o.documentSha256 ?? f.DOCUMENT_SHA256,
    sentAt: o.sentAt ?? o.createdAt,
    customerAcceptance: o.customerAcceptance,
    licensorAcceptance: o.licensorAcceptance,
    declined: o.declined ? { at: o.declined.at, note: o.declined.note } : null,
    voidedAt: o.voided?.at ?? null,
    authorized: o.terms.authorizedContacts.includes(me.user.email ?? ''),
    licenseId: o.licenseId,
  });
  const admin = { id: me.user.id, label: `${me.user.name} (${me.user.email ?? ''})` };
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
    myOrderForms: () => ({ items: [...orders.values()].filter((o) => o.status !== 'draft').map(mine) }),
    myOrderForm: ({ params }) => {
      const o = order(params.id);
      return o && o.status !== 'draft' ? { ...mine(o), authorized: true } : apiError(404, 'not_found', 'No such Order Form');
    },
    myOrderFormAccept: ({ params, body }) => {
      const b = body as AcceptOrderFormBody;
      const o = act(params.id, ['sent'], () => ({ status: 'accepted', customerAcceptance: { name: b.name, title: b.title, email: me.user.email, at: now(), documentSha256: b.documentSha256, userId: me.user.id, ip: '127.0.0.1', userAgent: navigator.userAgent } }));
      return 'terms' in o ? mine(o) : o;
    },
    myOrderFormDecline: ({ params, body }) => {
      const o = act(params.id, ['sent'], () => ({ status: 'declined', declined: { at: now(), by: me.user.name, note: (body as DeclineOrderFormBody).note } }));
      return 'terms' in o ? mine(o) : o;
    },
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
    licenseIssue: ({ body }) => {
      const id = (body as { orderFormId?: string }).orderFormId;
      const o = order(id);
      if (o) save(o, { licenseId: f.licenseDetail.id });
      return o ? { ...f.licenseDetail, orderForm: { id: o.id, number: o.number } } : f.licenseDetail;
    },
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
    orderFormsList: ({ query }) => ({
      items: [...orders.values()]
        .filter((o) => (!query.get('status') || o.status === query.get('status')) && (!query.get('customerId') || o.customer.id === query.get('customerId')))
        .map(f.summaryOf),
    }),
    orderFormGet: ({ params }) => order(params.id) ?? apiError(404, 'not_found', 'No such Order Form'),
    orderFormCreate: ({ body }) => {
      const b = body as CreateOrderFormBody;
      const n = orders.size + 1;
      const c = b.customerId ? { id: customer.id, name: customer.name, kind: customer.kind } : { id: `cus_new${n}`, name: b.newCustomer?.name ?? 'New organization', kind: 'organization' as const };
      return save(
        { ...f.draftOrderForm, id: `ord_new${n}`, number: `OF-2026-${String(n).padStart(4, '0')}`, customer: c, businessRequestId: b.businessRequestId ?? null, createdAt: now(), createdBy: admin },
        { terms: b.terms },
      );
    },
    orderFormUpdate: ({ params, body }) => act(params.id, ['draft'], (o) => ({ terms: { ...o.terms, ...(body as { terms: Partial<OrderFormTerms> }).terms } })),
    orderFormSend: ({ params }) => {
      const o = order(params.id);
      if (o && o.gaps.length) return apiError(400, 'invalid', 'The Order Form is not complete yet', { fields: o.gaps });
      return act(params.id, ['draft'], () => ({ status: 'sent', documentSha256: f.DOCUMENT_SHA256, sentAt: now(), sentBy: admin }));
    },
    orderFormRevise: ({ params }) => act(params.id, ['sent', 'declined'], (o) => ({ status: 'draft', version: o.version + 1, documentSha256: null, declined: null })),
    orderFormCountersign: ({ params, body }) =>
      act(params.id, ['accepted'], () => ({ status: 'executed', licensorAcceptance: { ...(body as { name: string; title: string; documentSha256: string }), email: me.user.email, at: now(), by: admin } })),
    orderFormVoid: ({ params, body }) => act(params.id, ['draft', 'sent', 'accepted', 'declined'], () => ({ status: 'void', voided: { at: now(), by: admin, reason: (body as { reason: string }).reason } })),
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
