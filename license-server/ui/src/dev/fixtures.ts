// Typed wire fixtures for the dev mock (./mock.ts) and the UI tests: one
// person per role, the catalog on a fixed day (2026-10-12, UTC; approval
// required, the default), licenses in each shape the pages render
// (personal and business, current and withdrawn), a customer, requests
// (a new key and a renewal), the preview of a one-click approval link,
// users, audit events and the System page. Every object is typed by
// shared/api.ts, so a wire change breaks the fixtures at compile time
// rather than the pages at run time.
import type {
  ApprovalPreview,
  AuditEvent,
  BusinessRequest,
  Catalog,
  CustomerDetail,
  LicenseDetail,
  LicenseRequest,
  LicenseSummary,
  Me,
  MyLicense,
  MyLicenseDetail,
  SystemInfo,
  UserDetail,
} from '@shared/api';
import { ATTESTATION_STATEMENT, FEATURES, PLANS, POLICY } from '@shared/catalog';
import { permissionsFor, type Role } from '@shared/permissions';

/** The day the fixtures live on (UTC). */
export const TODAY = '2026-10-12';

/** A key-shaped token (never a real signature). */
export const TOKEN =
  'eyJhbGciOiJFZERTQSIsImtpZCI6ImRldi0xIiwidHlwIjoiSldUIn0.eyJqdGkiOiJsaWNfZXhhbXBsZTEyIiwiaXNzIjoia3VkdXJydSIsInN1YiI6IkFkYSBMb3ZlbGFjZSIsInBsYW4iOiJwZXJzb25hbCIsIm5vZGVzIjozLCJpYXQiOjE3OTI0NDAwMDAsImV4cCI6MTgyMzk5MzE5OX0.c2lnbmF0dXJlLW5vdC1yZWFsLWp1c3QtYS1maXh0dXJlLWZvci10aGUtdWktdGVzdHMtMDEyMzQ1Njc4OWFiY2RlZg';

export function me(roles: Role[], over: Partial<Me['user']> = {}): Me {
  return {
    user: { id: roles.includes('superadmin') ? 'usr_super' : roles.includes('license_admin') ? 'usr_admin' : 'usr_ada', name: 'Ada Lovelace', email: 'ada@example.org', emailVerified: true, ...over },
    roles,
    permissions: permissionsFor(roles),
    csrf: 'csrf-token-1',
    devMode: false,
  };
}

export const customerMe = me(['customer']);
export const adminMe = me(['customer', 'license_admin'], { name: 'Alan Admin', email: 'admin@example.org' });
export const superMe = me(['customer', 'license_admin', 'superadmin'], { name: 'Sam Super', email: 'superadmin@example.org' });

export function catalog(over: Partial<Catalog> = {}): Catalog {
  return {
    plans: [...PLANS],
    features: [...FEATURES],
    policy: { ...POLICY },
    signing: { available: true, kid: 'kudurru-2026-1', trustedSince: 'v0.1.0', testKey: false },
    mail: { configured: true },
    today: TODAY,
    approvalRequired: true,
    ...over,
  };
}

export const myLicense: MyLicense = {
  id: 'lic_example12',
  customer: { id: 'cus_ada', name: 'Ada Lovelace' },
  licensee: 'Ada Lovelace',
  plan: 'personal',
  nodes: 3,
  features: null,
  expiresOn: '2027-10-12',
  state: 'active',
  keepsWorkingUntil: '2027-11-11',
  renewal: null,
};

export const myLicenseDetail: MyLicenseDetail = {
  ...myLicense,
  key: { kid: 'kudurru-2026-1', token: TOKEN, issuedAt: '2026-10-12T09:00:00.000Z', expiresOn: '2027-10-12' },
  worksWithKudurru: 'v0.1.0',
  people: [
    { email: 'ada@example.org', status: 'linked' },
    { email: 'charles@example.org', status: 'invited' },
  ],
  withdrawn: null,
  attestation: { source: 'portal', at: '2026-10-12T08:59:00.000Z', statement: 'I agree', license: 'LicenseRef-Kudurru-Community' },
};

export const myWithdrawnDetail: MyLicenseDetail = {
  ...myLicenseDetail,
  state: 'withdrawn',
  key: null,
  withdrawn: { at: '2026-09-01T10:00:00.000Z', keepsWorkingUntil: '2027-11-11', customerMessage: 'This key was used commercially.' },
};

const actor = { id: 'usr_admin', label: 'Alan Admin (admin@example.org)' };

export const licenseSummary: LicenseSummary = {
  id: 'lic_business01',
  customer: { id: 'cus_acme', name: 'Acme GmbH', kind: 'organization' },
  licensee: 'Acme GmbH',
  plan: 'business',
  nodes: 10,
  features: ['oidc', 'rbac'],
  expiresOn: '2027-10-12',
  state: 'active',
  keepsWorkingUntil: '2027-11-11',
  source: 'admin',
  createdAt: '2026-10-12T09:00:00.000Z',
  updatedAt: '2026-10-12T09:00:00.000Z',
};

export const licenseDetail: LicenseDetail = {
  ...licenseSummary,
  currentKey: { id: 'key_current', kid: 'kudurru-2026-1', token: TOKEN, issuedAt: '2026-10-12T09:00:00.000Z', expiresOn: '2027-10-12' },
  keys: [
    { id: 'key_current', kid: 'kudurru-2026-1', reason: 'renew', issuedAt: '2026-10-12T09:00:00.000Z', expiresOn: '2027-10-12', issuedBy: actor, current: true, supersededAt: null, supersededBy: null },
    { id: 'key_old', kid: 'kudurru-2026-1', reason: 'issue', issuedAt: '2025-10-12T09:00:00.000Z', expiresOn: '2026-10-12', issuedBy: actor, current: false, supersededAt: '2026-10-12T09:00:00.000Z', supersededBy: 'key_current' },
  ],
  attestation: null,
  agreementRef: 'PO-2026-17',
  notes: 'Annual contract.',
  createdBy: actor,
  worksWithKudurru: null,
  withdrawn: null,
};

export const withdrawnLicenseDetail: LicenseDetail = {
  ...licenseDetail,
  state: 'withdrawn',
  withdrawn: { at: '2026-09-01T10:00:00.000Z', by: actor, reason: 'Contract ended early', customerMessage: 'Your contract ended.', keepsWorkingUntil: '2027-11-11' },
};

export const customerDetail: CustomerDetail = {
  id: 'cus_acme',
  name: 'Acme GmbH',
  kind: 'organization',
  contactEmail: 'licenses@acme.example',
  licenseCount: 1,
  memberCount: 1,
  createdAt: '2026-01-02T09:00:00.000Z',
  updatedAt: '2026-10-12T09:00:00.000Z',
  notes: '',
  createdBy: actor,
  members: [{ email: 'ops@acme.example', status: 'linked', user: { id: 'usr_ops', name: 'Olga Ops', email: 'ops@acme.example' }, invitedAt: '2026-01-02T09:00:00.000Z', invitedBy: 'Alan Admin', linkedAt: '2026-01-03T09:00:00.000Z', inviteExpiresAt: null }],
  licenses: [licenseSummary],
  requests: [],
};

export const openRequest: LicenseRequest = {
  id: 'req_one',
  kind: 'personal',
  source: 'web',
  requestedBy: null,
  requesterEmail: 'grace@example.org',
  name: 'Grace Hopper',
  use: 'DNS for my homelab',
  attestation: { source: 'kudurru.io form', at: '2026-10-11T12:00:00.000Z', statement: 'I agree', license: 'LicenseRef-Kudurru-Community', email: 'grace@example.org' },
  reviewReason: 'auto_approve_off',
  status: 'open',
  decidedBy: null,
  decidedAt: null,
  decisionNote: '',
  resultingLicenseId: null,
  resultingKeyId: null,
  createdAt: '2026-10-11T12:00:00.000Z',
  personCustomer: null,
  renews: null,
};

/** A business request from the kudurru.io form that waits for a license admin. */
export const openBusinessRequest: BusinessRequest = {
  id: 'brq_one',
  name: 'Ada Admin',
  email: 'ada@example.org',
  company: 'Example GmbH',
  edition: 'enterprise',
  nodes: 12,
  details: 'OIDC sign-in and edge nodes in three offices; we would like to start in Q3.',
  status: 'open',
  decidedBy: null,
  decidedAt: null,
  decisionNote: '',
  resultingLicenseId: null,
  createdAt: '2026-10-12T08:30:00.000Z',
};

/** A business request a license admin answered with a license. */
export const issuedBusinessRequest: BusinessRequest = {
  ...openBusinessRequest,
  id: 'brq_two',
  name: 'Lin Ops',
  email: 'lin@initech.example',
  company: 'Initech',
  edition: 'core',
  nodes: 3,
  details: '',
  status: 'issued',
  decidedBy: { id: 'usr_admin', label: 'Alan Admin' },
  decidedAt: '2026-10-10T15:00:00.000Z',
  resultingLicenseId: 'lic_example12',
  createdAt: '2026-10-09T11:00:00.000Z',
};

/** A portal renewal waiting for approval: approving renews lic_example12 (expires 2026-10-30, inside the renew window). */
export const renewalRequest: LicenseRequest = {
  ...openRequest,
  id: 'req_renew',
  source: 'portal',
  requestedBy: { id: 'usr_ada', name: 'Ada Lovelace', email: 'ada@example.org' },
  requesterEmail: 'ada@example.org',
  name: 'Ada Lovelace',
  use: 'Renewal of lic_example12',
  attestation: { source: 'portal', at: '2026-10-12T08:00:00.000Z', statement: 'I agree', license: 'LicenseRef-Kudurru-Community', email: 'ada@example.org' },
  createdAt: '2026-10-12T08:00:00.000Z',
  personCustomer: { id: 'cus_ada', name: 'Ada Lovelace', kind: 'person' },
  renews: { licenseId: 'lic_example12', expiresOn: '2026-10-30' },
};

/** What /approve?token= shows for openRequest before anyone decided. */
export const approvalPreview: ApprovalPreview = {
  request: {
    id: openRequest.id,
    name: openRequest.name,
    email: openRequest.requesterEmail,
    use: openRequest.use,
    source: openRequest.source,
    createdAt: openRequest.createdAt,
    reviewReason: 'auto_approve_off',
    attestation: { at: '2026-10-11T12:00:00.000Z', statement: ATTESTATION_STATEMENT },
  },
  renews: null,
  terms: { plan: 'personal', nodes: 3, features: null, expiresOn: '2027-10-12' },
  status: 'open',
  decidedBy: null,
  expired: false,
  sentTo: 'admin@example.org',
};

export const userDetail: UserDetail = {
  id: 'usr_ops',
  name: 'Olga Ops',
  email: 'ops@acme.example',
  emailVerified: true,
  issuer: 'http://localhost:5556/dex',
  subject: 'CiQ4YjJhZ',
  idpRoles: ['customer'],
  grantedRoles: [],
  roles: ['customer'],
  disabled: false,
  disabledAt: null,
  createdAt: '2026-01-03T09:00:00.000Z',
  lastLoginAt: '2026-10-12T08:00:00.000Z',
  memberships: [{ customer: { id: 'cus_acme', name: 'Acme GmbH', kind: 'organization' }, email: 'ops@acme.example', linkedAt: '2026-01-03T09:00:00.000Z' }],
  activeSessions: 1,
};

export const auditEvents: AuditEvent[] = [
  { id: '42', at: '2026-10-12T09:00:00.000Z', actor, action: 'license.renewed', targetType: 'license', targetId: 'lic_business01', details: { expiresOn: '2027-10-12' }, ip: null, userAgent: null, requestId: null },
  { id: '41', at: '2026-10-11T09:00:00.000Z', actor, action: 'customer.created', targetType: 'customer', targetId: 'cus_acme', details: { name: 'Acme GmbH' }, ip: null, userAgent: null, requestId: null },
];

export const systemInfo: SystemInfo = {
  devMode: true,
  signing: { available: true, kid: 'dev-1', trusted: true, trustedSince: null, testKey: true, unavailableReason: null, trustedKids: ['dev-1', 'kudurru-2026-1'], keysByKid: [{ kid: 'dev-1', count: 3 }] },
  signIn: {
    issuer: 'http://localhost:5556/dex',
    clientId: 'license-portal',
    scopes: ['openid', 'profile', 'email', 'groups'],
    displayName: 'Sign in with Dex',
    groupsClaim: 'groups',
    idTokenAlg: null,
    roleMappings: { customer: [], license_admin: ['license-admins'], superadmin: ['license-superadmins'] },
    adminClaimRules: [{ claim: 'email_verified', value: 'true' }],
    defaultRole: 'customer',
    memberAutoLink: false,
  },
  sessions: { idleTimeoutSeconds: 1800, lifetimeSeconds: 43200 },
  mail: { configured: true, from: 'kudurru license portal <licenses@kudurru.io>', replyTo: 'hello@kudurru.io', adminNotify: null },
  publicForm: { enabled: true, origins: ['http://localhost:8792'], limitPerIpPerHour: 5, limitPerEmailPerDay: 3, personalAutoApprove: false, personalAutoApproveMaxPerDay: 50 },
  issuing: { maxPerActorPerDay: 50 },
  approval: { required: true, recipients: ['admin@example.org', 'superadmin@example.org'], fromAdmins: true },
  policy: { ...POLICY },
  build: { version: 'dev', commit: 'dev', builtAt: null },
};
