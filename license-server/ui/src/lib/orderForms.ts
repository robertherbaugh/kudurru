// The Order Form's terms as the portal's forms hold them (README, "Order
// Forms"; shared/orderForms.ts): every field's label (also for the gaps
// the server names as "terms.<field>"), the four sections the wizard steps
// and the detail page's inline sections follow, a draft that keeps numbers
// and US dollars as typed text, the checks the server repeats (the hard
// rules of orderFormRules; the gaps of orderFormGaps only block Send), and
// the download of the document as Markdown.
import type { OrderFormSignature, OrderFormTerms } from '@shared/api';
import { MAX_NODES, ORDER_FORM_MAX_CONTACTS, ORDER_FORM_MAX_FEE_CENTS, ORDER_FORM_MAX_TERM_DAYS } from '@shared/catalog';
import { addDays, type ExpiresOn } from '@shared/dates';
import { orderFormDocument, orderFormFileName, orderFormRules } from '@shared/orderForms';
import { listPrice, usd } from '@shared/pricing';
import { dateError } from '../ui/fields/DateField';
import { numberError } from '../ui/fields/NumberField';
import { emailError } from './validate';

export type TermsField = keyof OrderFormTerms;

/** Each field as the forms and the gap list name it. */
export const TERM_LABEL: Record<TermsField, string> = {
  effectiveDate: 'Effective Date',
  licensorContact: 'Licensor address and billing contact',
  customerLegalName: 'Customer legal name',
  customerAddress: 'Customer address',
  billingContact: 'Billing contact',
  authorizedContacts: 'Authorized contacts',
  edition: 'License plan',
  modules: 'Included Enterprise modules',
  namedModules: 'Named modules',
  nodes: 'Licensed Production Nodes',
  environments: 'Environments',
  permittedScope: 'Permitted clusters and entities',
  nonproductionNodes: 'Nonproduction and standby nodes',
  nonproductionTerms: 'Nonproduction terms',
  serviceProvider: 'Service Provider Use',
  serviceProviderScope: 'Service provider scope',
  government: 'Government use',
  governmentScope: 'Government scope',
  subscriptionStart: 'Subscription start',
  subscriptionEnd: 'Subscription end',
  platformFeeCents: 'Platform fee',
  perNodeFeeCents: 'Node fee',
  annualFeeCents: 'Annual fee',
  paymentTerms: 'Payment terms',
  keyPolicy: 'License key policy',
  support: 'Support and SLAs',
  renewal: 'Renewal',
  governingLaw: 'Governing law and venue',
  specialTerms: 'Special terms',
};

/** The label of a gap or a refused field: "terms.governingLaw" → "Governing law and venue". */
export function termLabel(path: string): string {
  const field = path.replace(/^terms\./, '').split('.')[0] as TermsField;
  return TERM_LABEL[field] ?? path;
}

export type TermsSection = 'parties' | 'plan' | 'term' | 'legal';

/** The sections in order: the wizard's steps and the detail page's inline sections. */
export const SECTION_FIELDS: Record<TermsSection, readonly TermsField[]> = {
  parties: ['customerLegalName', 'customerAddress', 'billingContact', 'authorizedContacts', 'licensorContact'],
  plan: ['edition', 'modules', 'namedModules', 'nodes', 'environments', 'permittedScope', 'nonproductionNodes', 'nonproductionTerms', 'serviceProvider', 'serviceProviderScope', 'government', 'governmentScope'],
  term: ['subscriptionStart', 'subscriptionEnd', 'effectiveDate', 'platformFeeCents', 'perNodeFeeCents', 'annualFeeCents', 'paymentTerms'],
  legal: ['keyPolicy', 'support', 'renewal', 'governingLaw', 'specialTerms'],
};

export const SECTION_TITLE: Record<TermsSection, string> = {
  parties: 'Parties and contacts',
  plan: 'Plan and scope',
  term: 'Term and fees',
  legal: 'Legal terms',
};

export const SECTIONS: readonly TermsSection[] = ['parties', 'plan', 'term', 'legal'];

/** Every terms field as a body path, in form order (for useFieldErrors / firstField). */
export const TERMS_PATHS: readonly string[] = SECTIONS.flatMap((s) => SECTION_FIELDS[s].map((f) => `terms.${f}`));

/** The section a body path ("terms.nodes") belongs to. */
export function sectionOf(path: string): TermsSection | null {
  const field = path.replace(/^terms\./, '').split('.')[0];
  return SECTIONS.find((s) => SECTION_FIELDS[s].some((f) => f === field)) ?? null;
}

/** The terms while they are edited: numbers and US dollars as typed, no Effective Date as ''. */
export type TermsDraft = Omit<OrderFormTerms, 'effectiveDate' | 'nodes' | 'nonproductionNodes' | 'platformFeeCents' | 'perNodeFeeCents' | 'annualFeeCents'> & {
  effectiveDate: string;
  nodes: string;
  nonproductionNodes: string;
  /** US dollars, e.g. "499.00". */
  platformFee: string;
  perNodeFee: string;
  annualFee: string;
};

/** Cents as the dollars a person types: 199400 → "1994.00". */
export function centsToDollars(cents: number): string {
  return (cents / 100).toFixed(2);
}

/** Typed US dollars ("1,994", "$1994.5") as whole cents, or null when it is not an amount. */
export function dollarsToCents(text: string): number | null {
  const t = text.trim().replace(/^\$/, '').replace(/,/g, '');
  if (!/^\d+(\.\d{1,2})?$/.test(t)) return null;
  return Math.round(Number(t) * 100);
}

/** A user-facing error for an amount in US dollars, or null. */
export function dollarsError(text: string): string | null {
  if (!text.trim()) return 'Enter an amount in US dollars, e.g. 1994.00';
  const cents = dollarsToCents(text);
  if (cents === null) return 'An amount in US dollars, e.g. 1994.00';
  if (cents > ORDER_FORM_MAX_FEE_CENTS) return `At most ${usd(ORDER_FORM_MAX_FEE_CENTS)}`;
  return null;
}

export function termsToDraft(t: OrderFormTerms): TermsDraft {
  const { effectiveDate, nodes, nonproductionNodes, platformFeeCents, perNodeFeeCents, annualFeeCents, ...rest } = t;
  return {
    ...rest,
    authorizedContacts: [...t.authorizedContacts],
    namedModules: [...t.namedModules],
    effectiveDate: effectiveDate ?? '',
    nodes: String(nodes),
    nonproductionNodes: String(nonproductionNodes),
    platformFee: centsToDollars(platformFeeCents),
    perNodeFee: centsToDollars(perNodeFeeCents),
    annualFee: centsToDollars(annualFeeCents),
  };
}

function wholeOr(text: string, fallback: number): number {
  const t = text.trim();
  return /^\d+$/.test(t) ? Number(t) : fallback;
}

/** The terms of a draft (text trimmed, empty contacts dropped); numbers that do not parse become 0, which the checks refuse first. */
export function draftToTerms(d: TermsDraft): OrderFormTerms {
  const { effectiveDate, nodes, nonproductionNodes, platformFee, perNodeFee, annualFee, ...rest } = d;
  const trimmed = Object.fromEntries(Object.entries(rest).map(([k, v]) => [k, typeof v === 'string' ? v.trim() : v])) as typeof rest;
  return {
    ...trimmed,
    authorizedContacts: d.authorizedContacts.map((c) => c.trim()).filter(Boolean),
    namedModules: d.modules === 'named' ? [...d.namedModules] : [],
    effectiveDate: effectiveDate.trim() || null,
    nodes: wholeOr(nodes, 0),
    nonproductionNodes: wholeOr(nonproductionNodes, 0),
    platformFeeCents: dollarsToCents(platformFee) ?? 0,
    perNodeFeeCents: dollarsToCents(perNodeFee) ?? 0,
    annualFeeCents: dollarsToCents(annualFee) ?? 0,
  };
}

/** The draft's terms of one section only (an inline section's PATCH). */
export function sectionTerms(d: TermsDraft, section: TermsSection): Partial<OrderFormTerms> {
  const all = draftToTerms(d);
  return Object.fromEntries(SECTION_FIELDS[section].map((f) => [f, all[f]]));
}

/** The latest subscription end for a start day. */
export function maxSubscriptionEnd(start: ExpiresOn): ExpiresOn | undefined {
  return dateError(start) ? undefined : addDays(start, ORDER_FORM_MAX_TERM_DAYS);
}

/**
 * What the server refuses on save, by field: numbers out of range, amounts
 * that are not dollars, contacts that are not email addresses, dates, and
 * the hard rules (orderFormRules). Empty text is a gap, not an error.
 */
export function draftErrors(d: TermsDraft): Partial<Record<TermsField, string>> {
  const out: Partial<Record<TermsField, string>> = {};
  const contacts = d.authorizedContacts.map((c) => c.trim()).filter(Boolean);
  const badContact = contacts.find((c) => emailError(c));
  if (badContact) out.authorizedContacts = `${badContact}: not an email address`;
  else if (contacts.length > ORDER_FORM_MAX_CONTACTS) out.authorizedContacts = `At most ${ORDER_FORM_MAX_CONTACTS} contacts`;
  else if (new Set(contacts.map((c) => c.toLowerCase())).size !== contacts.length) out.authorizedContacts = 'Each contact once';
  const nodes = numberError(d.nodes, { min: 1, max: MAX_NODES });
  if (nodes) out.nodes = nodes;
  const nonprod = numberError(d.nonproductionNodes, { min: 0, max: MAX_NODES });
  if (nonprod) out.nonproductionNodes = nonprod;
  for (const [field, text] of [
    ['platformFeeCents', d.platformFee],
    ['perNodeFeeCents', d.perNodeFee],
    ['annualFeeCents', d.annualFee],
  ] as const) {
    const e = dollarsError(text);
    if (e) out[field] = e;
  }
  const start = dateError(d.subscriptionStart);
  if (start) out.subscriptionStart = start;
  const end = dateError(d.subscriptionEnd);
  if (end) out.subscriptionEnd = end;
  const eff = dateError(d.effectiveDate, { optional: true });
  if (eff) out.effectiveDate = eff;
  if (!start && !end && !eff && !nodes) {
    for (const p of orderFormRules(draftToTerms(d))) out[p.field] ??= p.message;
  }
  return out;
}

/** The first error of a section, as "<label>: <message>", or null. */
export function sectionError(d: TermsDraft, section: TermsSection): string | null {
  const errs = draftErrors(d);
  const field = SECTION_FIELDS[section].find((f) => errs[f]);
  return field ? `${TERM_LABEL[field]}: ${errs[field]}` : null;
}

/** The fees of the price list for the draft's edition and nodes, or null for a quote (50 or more nodes). */
export function listFees(d: Pick<TermsDraft, 'edition' | 'nodes'>): { platformFee: string; perNodeFee: string; annualFee: string } | null {
  const p = listPrice(d.edition, Number(d.nodes.trim() || 'NaN'));
  return p ? { platformFee: centsToDollars(p.platformCents), perNodeFee: centsToDollars(p.perNodeCents), annualFee: centsToDollars(p.annualCents) } : null;
}

/** Whether the draft's fees are the price list's for its edition and nodes. */
export function feesFollowList(d: TermsDraft): boolean {
  const l = listFees(d);
  if (!l) return false;
  return dollarsToCents(d.platformFee) === dollarsToCents(l.platformFee) && dollarsToCents(d.perNodeFee) === dollarsToCents(l.perNodeFee) && dollarsToCents(d.annualFee) === dollarsToCents(l.annualFee);
}

/**
 * A change to the draft with what follows from it: a new edition keeps the
 * modules valid (Enterprise includes all modules, Commercial Core none), and
 * with `followList` fees that were the price list's follow a new edition or
 * node count.
 */
export function patchDraft(d: TermsDraft, patch: Partial<TermsDraft>, opts: { followList?: boolean } = {}): TermsDraft {
  let next: TermsDraft = { ...d, ...patch };
  if (patch.edition && patch.edition !== d.edition) {
    if (next.edition === 'enterprise' && next.modules === 'none') next = { ...next, modules: 'all' };
    if (next.edition === 'core' && next.modules === 'all') next = { ...next, modules: 'none' };
  }
  if (patch.modules && patch.modules !== 'named') next = { ...next, namedModules: [] };
  if (opts.followList && (patch.edition !== undefined || patch.nodes !== undefined) && feesFollowList(d)) {
    const l = listFees(next);
    if (l) next = { ...next, ...l };
  }
  return next;
}

/** What an Order Form's document is rendered from (the admin's and the customer's DTOs both fit). */
export interface DocumentSource {
  number: string;
  version: number;
  terms: OrderFormTerms;
  documentSha256: string | null;
  customerAcceptance: OrderFormSignature | null;
  licensorAcceptance: OrderFormSignature | null;
}

function signatureOf(s: OrderFormSignature | null): OrderFormSignature | null {
  return s ? { name: s.name, title: s.title, email: s.email, at: s.at, documentSha256: s.documentSha256 } : null;
}

/** The document text: the hashed text, with both acceptances once there is any (the copy each party keeps). */
export function documentText(f: DocumentSource): string {
  const signed = (f.customerAcceptance || f.licensorAcceptance) && f.documentSha256;
  return orderFormDocument({
    number: f.number,
    version: f.version,
    terms: f.terms,
    ...(signed ? { signatures: { customer: signatureOf(f.customerAcceptance), licensor: signatureOf(f.licensorAcceptance), documentSha256: f.documentSha256 as string } } : {}),
  });
}

/** Saves the document as OF-2026-0001.md through a Blob link. */
export function downloadOrderForm(f: DocumentSource): void {
  const blob = new Blob([documentText(f)], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = orderFormFileName(f.number);
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** Schedule A's modules without the Markdown code marks. */
export function plainWords(s: string): string {
  return s.replace(/`/g, '');
}
