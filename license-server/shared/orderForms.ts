// Order Forms (README, "Order Forms"): Schedule A of
// COMMERCIAL_LICENSE_AGREEMENT.md as data. The server and the UI share the
// defaults of a new form, the rules a form must meet before it is sent
// (orderFormRules, orderFormGaps) and the document text itself
// (orderFormDocument): the text sent is stored and hashed (SHA-256, server
// side), each party accepts that hash, and both download that stored text.
// A changed rendering changes only forms sent afterwards: a sent form keeps
// the text it was sent with (signedOrderFormCopy adds the acceptances).

import type { OrderFormSignature, OrderFormTerms } from './api';
import {
  BUSINESS_DEFAULT_TERM_DAYS,
  BUSINESS_EDITIONS,
  COMMERCIAL_AGREEMENT_URL,
  featureLabel,
  LICENSOR_NAME,
  ORDER_FORM_MAX_TERM_DAYS,
  type BusinessEdition,
  type Feature,
} from './catalog';
import { addDays, daysBetween, type ExpiresOn } from './dates';
import { listPrice, usd } from './pricing';

/** The defaults of Schedule A's text fields, from the agreement's own terms. */
export const ORDER_FORM_DEFAULTS = Object.freeze({
  environments: 'all covered environments and clusters',
  permittedScope: 'All customer-owned clusters and entities, subject to the Node cap',
  nonproductionTerms: 'No additional running Nodes are included beyond the purchased count (Section 4.3).',
  paymentTerms: 'Invoiced annually in advance, due within 30 calendar days of invoice, in USD (Section 5.2).',
  keyPolicy: 'Keys are delivered through the kudurru license portal and verified offline on each Node; no Node contacts the licensor.',
  support: 'None. No support or SLA is included.',
  renewal: 'None. Renewal requires a new accepted Order Form or other authorized agreement (Section 13.1).',
});

/** What a new form starts from (a business request or a customer). */
export interface OrderFormSeed {
  edition: BusinessEdition;
  nodes: number;
  customerLegalName: string;
  /** The first authorized contact, also the billing contact. */
  contact?: string | null;
  /** The subscription starts here (today, UTC). */
  today: ExpiresOn;
  /** The licensor's address of the last form, when there is one. */
  licensorContact?: string;
}

/** The terms of a new form: the edition's modules, a one-year subscription from today, the fees of the price list. */
export function defaultOrderFormTerms(seed: OrderFormSeed): OrderFormTerms {
  const price = listPrice(seed.edition, seed.nodes);
  const contact = seed.contact?.trim() ?? '';
  return {
    effectiveDate: null,
    licensorContact: seed.licensorContact ?? '',
    customerLegalName: seed.customerLegalName,
    customerAddress: '',
    billingContact: contact,
    authorizedContacts: contact ? [contact] : [],
    edition: seed.edition,
    modules: seed.edition === 'enterprise' ? 'all' : 'none',
    namedModules: [],
    nodes: seed.nodes,
    environments: ORDER_FORM_DEFAULTS.environments,
    permittedScope: ORDER_FORM_DEFAULTS.permittedScope,
    nonproductionNodes: 0,
    nonproductionTerms: ORDER_FORM_DEFAULTS.nonproductionTerms,
    serviceProvider: 'not_authorized',
    serviceProviderScope: '',
    government: 'no',
    governmentScope: '',
    subscriptionStart: seed.today,
    subscriptionEnd: addDays(seed.today, BUSINESS_DEFAULT_TERM_DAYS - 1),
    platformFeeCents: price?.platformCents ?? 0,
    perNodeFeeCents: price?.perNodeCents ?? 0,
    annualFeeCents: price?.annualCents ?? 0,
    paymentTerms: ORDER_FORM_DEFAULTS.paymentTerms,
    keyPolicy: ORDER_FORM_DEFAULTS.keyPolicy,
    support: ORDER_FORM_DEFAULTS.support,
    renewal: ORDER_FORM_DEFAULTS.renewal,
    governingLaw: '',
    specialTerms: '',
  };
}

/** A broken rule: the body path of the field it shows under and one sentence. */
export interface OrderFormProblem {
  field: keyof OrderFormTerms;
  message: string;
}

/**
 * The rules every stored form meets, draft or not (the server answers 400
 * with `terms.<field>` for each): the modules fit the edition, named modules
 * name at least one, the subscription ends after it starts and within ten
 * years, the effective date is not after the end.
 */
export function orderFormRules(t: OrderFormTerms): OrderFormProblem[] {
  const out: OrderFormProblem[] = [];
  if (t.edition === 'enterprise' && t.modules === 'none') out.push({ field: 'modules', message: 'An Enterprise plan includes all current modules or named modules.' });
  if (t.edition === 'core' && t.modules === 'all') out.push({ field: 'modules', message: 'All current modules is the Enterprise plan: choose Enterprise, or name the modules.' });
  if (t.modules === 'named' && t.namedModules.length === 0) out.push({ field: 'namedModules', message: 'Name at least one module.' });
  if (t.modules !== 'named' && t.namedModules.length > 0) out.push({ field: 'namedModules', message: 'Modules are named only when the form includes named modules.' });
  if (t.subscriptionEnd <= t.subscriptionStart) out.push({ field: 'subscriptionEnd', message: 'The subscription ends after it starts.' });
  else if (daysBetween(t.subscriptionStart, t.subscriptionEnd) > ORDER_FORM_MAX_TERM_DAYS) out.push({ field: 'subscriptionEnd', message: 'A subscription runs at most ten years.' });
  if (t.effectiveDate !== null && t.effectiveDate > t.subscriptionEnd) out.push({ field: 'effectiveDate', message: 'The Effective Date is not after the subscription ends.' });
  return out;
}

const REQUIRED_TEXT: readonly (keyof OrderFormTerms)[] = [
  'licensorContact',
  'customerLegalName',
  'customerAddress',
  'billingContact',
  'environments',
  'permittedScope',
  'paymentTerms',
  'keyPolicy',
  'support',
  'renewal',
  'governingLaw',
];

/** What a draft still lacks before it can be sent (empty text, no authorized contact, a scope left open), as field names. */
export function orderFormGaps(t: OrderFormTerms): (keyof OrderFormTerms)[] {
  const out: (keyof OrderFormTerms)[] = REQUIRED_TEXT.filter((k) => String(t[k]).trim() === '');
  if (t.authorizedContacts.length === 0) out.push('authorizedContacts');
  if (t.serviceProvider === 'authorized' && t.serviceProviderScope.trim() === '') out.push('serviceProviderScope');
  if (t.government === 'yes' && t.governmentScope.trim() === '') out.push('governmentScope');
  return out;
}

/**
 * The features a key issued from the form carries: null (every feature) for
 * all current modules, the named modules, or [] for none. A key cannot list
 * no feature (that would be every feature), so for [] the license admin
 * chooses what the agreement covers.
 */
export function keyFeaturesFor(t: Pick<OrderFormTerms, 'modules' | 'namedModules'>): Feature[] | null {
  if (t.modules === 'all') return null;
  if (t.modules === 'named') return [...t.namedModules];
  return [];
}

/** "Commercial Core" or "Enterprise". */
export function editionLabel(edition: BusinessEdition): string {
  return BUSINESS_EDITIONS[edition].label;
}

/** Schedule A's "Included Enterprise modules", in words. */
export function modulesInWords(t: Pick<OrderFormTerms, 'modules' | 'namedModules'>): string {
  if (t.modules === 'all') return 'All current `ee/` modules';
  if (t.modules === 'none') return 'None';
  return `Named modules: ${t.namedModules.map(featureLabel).join(', ')}`;
}

/** The fees in words: the annual fee, then how the price list composes it. */
export function feesInWords(t: Pick<OrderFormTerms, 'annualFeeCents' | 'platformFeeCents' | 'perNodeFeeCents' | 'nodes'>): string {
  return `${usd(t.annualFeeCents)} per year (platform fee ${usd(t.platformFeeCents)}; Node fee ${usd(t.perNodeFeeCents)} per Node)`;
}

/** Text for one table cell: one line, pipes escaped. */
function cell(s: string): string {
  return s.trim().replace(/\r\n?/g, '\n').split('\n').map((l) => l.trim()).filter((l) => l !== '').join('; ').replace(/\|/g, '\\|');
}

/** A block of lines, each indented under its label. */
function block(s: string): string {
  if (s.trim() === '') return '';
  return s.trim().replace(/\r\n?/g, '\n').split('\n').map((l) => `  ${l.trim()}`).join('\n');
}

/** An instant as the document prints it: 2026-10-18 09:41 UTC. */
function utcMinute(iso: string): string {
  return `${iso.slice(0, 16).replace('T', ' ')} UTC`;
}

function signatureLine(s: OrderFormSignature): string {
  return `${cell(s.name)}, ${cell(s.title)}${s.email ? ` (${s.email})` : ''}, accepted electronically on ${utcMinute(s.at)}`;
}

/** What the document is rendered from. */
export interface OrderFormDocumentInput {
  number: string;
  version: number;
  terms: OrderFormTerms;
  /** Both acceptances, for the kept copy; the text that is hashed has none. */
  signatures?: OrderFormSignatures;
}

/**
 * The Order Form as a Markdown document: Schedule A with the agreed terms.
 * Without `signatures` it is the text that is hashed and accepted; with them
 * it is the copy each party keeps.
 */
export function orderFormDocument(input: OrderFormDocumentInput): string {
  const t = input.terms;
  const rows: [string, string][] = [
    ['License plan', editionLabel(t.edition)],
    ['Included Enterprise modules', modulesInWords(t)],
    ['Licensed Production Nodes', `${t.nodes} concurrent running ${t.nodes === 1 ? 'Node' : 'Nodes'} across ${cell(t.environments)}`],
    ['Permitted clusters / entities', cell(t.permittedScope)],
    ['Nonproduction / standby Nodes', `${t.nonproductionNodes}${t.nonproductionTerms.trim() ? `. ${cell(t.nonproductionTerms)}` : ''}`],
    ['Service Provider Use', t.serviceProvider === 'authorized' ? `Authorized: ${cell(t.serviceProviderScope)}` : 'Not authorized'],
    ['Government use', t.government === 'yes' ? `Yes: ${cell(t.governmentScope)}` : 'No'],
    ['Internal documentation mirror', 'Included for Authorized Users under Section 3.3'],
    ['Website and license-server source', 'Excluded (`website/` and `license-server/`); no deployment or modification rights'],
    ['Subscription Start / End', `${t.subscriptionStart} to ${t.subscriptionEnd} (UTC)`],
    ['Annual fees / Node fees', `${feesInWords(t)}. ${cell(t.paymentTerms)}`],
    ['License key policy', cell(t.keyPolicy)],
    ['Support / SLAs', cell(t.support)],
    ['Renewal', cell(t.renewal)],
    ['Governing law and venue', cell(t.governingLaw)],
    ['Special terms', t.specialTerms.trim() ? cell(t.specialTerms) : 'None'],
  ];
  const lines = [
    '# Kudurru Order Form',
    '',
    `Order Form No.: ${input.number} (version ${input.version})`,
    '',
    `Effective Date: ${t.effectiveDate ?? t.subscriptionStart}`,
    '',
    `This Order Form is entered into under the Kudurru Commercial Software License Agreement (${COMMERCIAL_AGREEMENT_URL}), which it incorporates. It becomes binding when both parties have accepted it.`,
    '',
    `Licensor: ${LICENSOR_NAME}`,
    block(t.licensorContact),
    '',
    `Customer: ${cell(t.customerLegalName)}`,
    block(t.customerAddress),
    '',
    `Billing contact: ${cell(t.billingContact)}`,
    '',
    `Authorized contacts: ${t.authorizedContacts.join(', ')}`,
    '',
    '| Field | Agreed terms |',
    '|---|---|',
    ...rows.map(([k, v]) => `| ${k} | ${v} |`),
    '',
  ];
  const text = lines.join('\n').replace(/\n{3,}/g, '\n\n');
  return input.signatures ? signedOrderFormCopy(text, input.signatures) : text;
}

/** The acceptances of both parties, as the kept copy prints them below the text. */
export interface OrderFormSignatures {
  customer: OrderFormSignature | null;
  licensor: OrderFormSignature | null;
  documentSha256: string;
}

/**
 * The copy each party keeps: the text that was sent and hashed (as stored
 * when it was sent, so later template changes never alter it), followed by
 * both acceptances and the hash.
 */
export function signedOrderFormCopy(text: string, s: OrderFormSignatures): string {
  return [
    text,
    '## Acceptance',
    '',
    `Customer acceptance: ${s.customer ? signatureLine(s.customer) : 'not yet accepted'}`,
    '',
    `Licensor acceptance: ${s.licensor ? signatureLine(s.licensor) : 'not yet accepted'}`,
    '',
    `Document SHA-256: ${s.documentSha256}`,
    '',
  ].join('\n');
}

/** The file name of the document: OF-2026-0001.md. */
export function orderFormFileName(number: string): string {
  return `${number}.md`;
}
