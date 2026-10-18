// "New Order Form" (POST /api/v1/order-forms) in a WizardDialog over the
// page the admin is on (dev-docs/milestones/m3/ui.md: wizards only for
// creation), in six steps that follow Schedule A:
// 1. Customer: an existing one (CustomerField) or a new organization;
// 2. Contacts and parties: the customer's legal name, address, billing
//    contact and authorized contacts, the licensor's address;
// 3. Plan and scope: edition, modules, nodes, environments and scope,
//    nonproduction nodes, service provider and government use;
// 4. Term and fees: the subscription and Effective Date, the fees in US
//    dollars from the price list (shared/pricing) unless agreed otherwise,
//    the payment terms;
// 5. Legal terms: key policy, support, renewal, governing law, special terms;
// 6. Review, then "Create draft", which opens the new form's page.
// A draft may have gaps (orderFormGaps): the review lists what Send will
// still need; only the hard rules (orderFormRules) and malformed values
// block Create. Opened from a business request, it starts with a new
// organization named after the company, the requester as contact, the
// edition and the nodes asked for; from a customer's page, that customer.
// The fees follow the price list while they are the list's.
import { Banner } from '@astryxdesign/core/Banner';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useRef, useState } from 'react';
import type { BusinessRequest, CreateOrderFormBody } from '@shared/api';
import { BUSINESS_EDITIONS } from '@shared/catalog';
import { todayUtc } from '@shared/dates';
import { defaultOrderFormTerms, editionLabel, feesInWords, modulesInWords, orderFormGaps } from '@shared/orderForms';
import { firstField, useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useRouteMutation } from '../api/hooks';
import { utcDay } from '../lib/dates';
import { draftToTerms, patchDraft, plainWords, SECTION_TITLE, sectionError, sectionOf, TERM_LABEL, TERMS_PATHS, termsToDraft, type TermsDraft, type TermsField, type TermsSection } from '../lib/orderForms';
import { emailError, nameError } from '../lib/validate';
import { useNotify } from '../ui/components/Feedback';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../ui/components/Wizard';
import { ChoiceCards } from '../ui/fields/ChoiceCards';
import { CustomerField, type CustomerChoice } from '../ui/fields/CustomerField';
import { TextField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';
import { useNavigate } from '../ui/lib/router';
import { TermsFields } from './OrderTerms';
import { WizardNavHandle, type WizardNav } from './WizardNavHandle';

export interface OrderFormWizardProps {
  isOpen: boolean;
  onClose: () => void;
  /** Opened from a customer's page: that customer, chosen. */
  customer?: CustomerChoice | undefined;
  /** Opened from a business request: its company, contact, edition and nodes filled in. */
  request?: BusinessRequest | undefined;
}

export function OrderFormWizardDialog({ isOpen, onClose, customer, request }: OrderFormWizardProps) {
  return (
    <WizardDialog
      isOpen={isOpen}
      onClose={onClose}
      title="New Order Form"
      subtitle={request ? `Drafts Schedule A for the business request of ${request.company}.` : 'Drafts Schedule A of the commercial agreement for a customer.'}
      what="Order Form"
    >
      <OrderFormWizard onClose={onClose} customer={customer} request={request} />
    </WizardDialog>
  );
}

const CUSTOMER_FIELDS = ['customerId', 'newCustomer.name', 'newCustomer.kind', 'newCustomer.contactEmail'];

/** The body fields the steps' controls show, in step order. */
const CREATE_FIELDS = [...CUSTOMER_FIELDS, ...TERMS_PATHS];

const TERM_STEPS: readonly TermsSection[] = ['parties', 'plan', 'term', 'legal'];

const STEP_DESCRIPTION: Record<TermsSection, string> = {
  parties: 'Who the form binds and who may accept it. Sending makes each authorized contact a member of the customer.',
  plan: 'The license plan and what it covers. A key issued from the form carries the plan, the nodes and the modules.',
  term: 'The subscription and what it costs a year. Fees start from the price list; you may agree others.',
  legal: 'The terms Schedule A records next to the agreement. Defaults come from the agreement itself.',
};

/** The customer fields a new form copies from the customer while the admin has not typed them. */
type Followed = 'customerLegalName' | 'billingContact' | 'authorizedContacts';

function OrderFormWizard({ onClose, customer: preset, request }: { onClose: () => void; customer?: CustomerChoice | undefined; request?: BusinessRequest | undefined }) {
  const catalog = useCatalog();
  const notify = useNotify();
  const navigate = useNavigate();
  const m = useRouteMutation('orderFormCreate');
  const fe = useFieldErrors(m.error, CREATE_FIELDS);
  const navRef = useRef<WizardNav | null>(null);
  const today = catalog.data?.today ?? todayUtc();

  const [mode, setMode] = useState<'existing' | 'new'>(request ? 'new' : 'existing');
  const [customer, setCustomer] = useState<CustomerChoice | null>(preset ?? null);
  const [newName, setNewName] = useState(request?.company ?? '');
  const [newEmail, setNewEmail] = useState(request?.email ?? '');
  const [draft, setDraft] = useState<TermsDraft>(() =>
    termsToDraft(
      defaultOrderFormTerms({
        edition: request?.edition ?? 'core',
        nodes: request?.nodes ?? 1,
        customerLegalName: request?.company ?? preset?.name ?? '',
        contact: request?.email ?? preset?.contactEmail ?? null,
        today,
      }),
    ),
  );
  const [typed, setTyped] = useState<ReadonlySet<Followed>>(new Set());
  const [showErrors, setShowErrors] = useState<Record<string, boolean>>({});
  const [dirty, setDirty] = useState(false);

  const change = (patch: Partial<TermsDraft>) => {
    setDirty(true);
    const keys = Object.keys(patch).filter((k): k is Followed => k === 'customerLegalName' || k === 'billingContact' || k === 'authorizedContacts');
    if (keys.length) setTyped((s) => new Set([...s, ...keys]));
    setDraft((d) => patchDraft(d, patch, { followList: true }));
  };

  // The legal name and the contacts follow the customer until the admin types them.
  const follow = (name: string, contact: string | null) => {
    setDraft((d) => ({
      ...d,
      ...(typed.has('customerLegalName') ? {} : { customerLegalName: name }),
      ...(typed.has('billingContact') ? {} : { billingContact: contact ?? '' }),
      ...(typed.has('authorizedContacts') ? {} : { authorizedContacts: contact ? [contact] : [] }),
    }));
  };

  const customerError = (): string | null => {
    if (mode === 'existing') return customer ? null : 'Choose a customer, or create a new organization';
    return nameError(newName, 'the organization name') ?? emailError(newEmail, { optional: true });
  };

  const firstProblem = (): { step: string; message: string } | null => {
    const c = customerError();
    if (c) return { step: 'customer', message: c };
    for (const s of TERM_STEPS) {
      const message = sectionError(draft, s);
      if (message) return { step: s, message };
    }
    return null;
  };

  const terms = draftToTerms(draft);
  const gaps = orderFormGaps(terms);
  const customerName = mode === 'existing' ? (customer?.name ?? '') : newName.trim();

  const create = async () => {
    if (firstProblem() || (mode === 'existing' && !customer)) return;
    const base = { terms, ...(request ? { businessRequestId: request.id } : {}) };
    const body: CreateOrderFormBody =
      mode === 'existing' && customer ? { ...base, customerId: customer.id } : { ...base, newCustomer: { name: newName.trim(), kind: 'organization', contactEmail: newEmail.trim() || null } };
    try {
      const f = await m.mutateAsync({ body });
      notify.success(`Drafted ${f.number} for ${f.customer.name}`);
      onClose();
      navigate(`/orders/${encodeURIComponent(f.id)}`);
    } catch (err) {
      const field = firstField(err, CREATE_FIELDS);
      if (field) navRef.current?.goTo(CUSTOMER_FIELDS.includes(field) ? 'customer' : (sectionOf(field) ?? 'review'));
    }
  };

  const steps: WizardStep[] = [
    {
      id: 'customer',
      label: 'Customer',
      description: 'Who the form is for. Its authorized contacts become members of this customer when it is sent.',
      content: (
        <VStack gap={4}>
          <ChoiceCards
            label="Customer"
            isLabelHidden
            value={mode}
            onChange={(v) => {
              setMode(v as 'existing' | 'new');
              setDirty(true);
              fe.edited('customerId');
              if (v === 'existing') follow(customer?.name ?? '', customer?.contactEmail ?? null);
              else follow(newName, newEmail.trim() || null);
            }}
            choices={[
              { value: 'existing', label: 'Existing customer', description: 'Search by name, contact email or member email.' },
              { value: 'new', label: 'New organization', description: 'Created together with the draft.' },
            ]}
          />
          {mode === 'existing' ? (
            <CustomerField
              value={customer}
              onChange={(c) => {
                setCustomer(c);
                setDirty(true);
                fe.edited('customerId');
                follow(c?.name ?? '', c?.contactEmail ?? null);
              }}
              isRequired
              error={(showErrors.customer && !customer ? 'Choose a customer' : null) ?? fe.field('customerId')}
            />
          ) : (
            <VStack gap={3}>
              <TextField
                label="Organization name"
                value={newName}
                onChange={(v) => {
                  setNewName(v);
                  fe.edited('newCustomer.name');
                  follow(v, newEmail.trim() || null);
                }}
                isRequired
                error={(showErrors.customer ? nameError(newName, 'the organization name') : null) ?? fe.field('newCustomer.name')}
              />
              <TextField
                label="Contact email"
                type="email"
                value={newEmail}
                onChange={(v) => {
                  setNewEmail(v);
                  fe.edited('newCustomer.contactEmail');
                  follow(newName, v.trim() || null);
                }}
                isOptional
                help="The customer's contact; also the first authorized contact and the billing contact unless you change them."
                error={(showErrors.customer ? emailError(newEmail, { optional: true }) : null) ?? fe.field('newCustomer.contactEmail', 'newCustomer.kind')}
              />
            </VStack>
          )}
        </VStack>
      ),
      validate: () => {
        setShowErrors((s) => ({ ...s, customer: true }));
        return customerError();
      },
    },
    ...TERM_STEPS.map(
      (s): WizardStep => ({
        id: s,
        label: s === 'parties' ? 'Contacts and parties' : SECTION_TITLE[s],
        description: STEP_DESCRIPTION[s],
        content: <TermsFields section={s} draft={draft} onChange={change} showErrors={Boolean(showErrors[s])} serverError={(p) => fe.field(p)} edited={fe.edited} />,
        validate: () => {
          setShowErrors((e) => ({ ...e, [s]: true }));
          return sectionError(draft, s);
        },
      }),
    ),
    {
      id: 'review',
      label: 'Review',
      description: 'The draft is created as it stands; you can change it on its page until you send it.',
      content: (
        <VStack gap={3}>
          <WizardNavHandle navRef={navRef} />
          <MetadataList>
            {request ? (
              <MetadataListItem label="Business request">
                {request.company} ({request.email}): {BUSINESS_EDITIONS[request.edition].label}, {formatExact(request.nodes)} {request.nodes === 1 ? 'node' : 'nodes'}.
              </MetadataListItem>
            ) : null}
            <ReviewRow label="Customer" step="customer" what="customer">
              {mode === 'existing' ? customerName : `${customerName} (new organization${newEmail.trim() ? `, ${newEmail.trim()}` : ''})`}
            </ReviewRow>
            <ReviewRow label={TERM_LABEL.customerLegalName} step="parties" what="legal name">
              {terms.customerLegalName || '–'}
            </ReviewRow>
            <ReviewRow label={TERM_LABEL.authorizedContacts} step="parties" what="authorized contacts">
              {terms.authorizedContacts.join(', ') || '–'}
            </ReviewRow>
            <ReviewRow label={TERM_LABEL.edition} step="plan" what="plan">
              {editionLabel(terms.edition)}: {plainWords(modulesInWords(terms))}
            </ReviewRow>
            <ReviewRow label={TERM_LABEL.nodes} step="plan" what="nodes">
              {formatExact(terms.nodes)}
            </ReviewRow>
            <ReviewRow label="Subscription" step="term" what="subscription">
              {utcDay(terms.subscriptionStart)} to {utcDay(terms.subscriptionEnd)}
            </ReviewRow>
            <ReviewRow label="Fees" step="term" what="fees">
              {feesInWords(terms)}
            </ReviewRow>
            <ReviewRow label={TERM_LABEL.governingLaw} step="legal" what="governing law">
              {terms.governingLaw || '–'}
            </ReviewRow>
          </MetadataList>
          {gaps.length ? (
            <Banner status="info" title="Sending will still need" description={gaps.map((g: TermsField) => TERM_LABEL[g]).join(', ')} collapsible={false} />
          ) : (
            <Text type="supporting">Nothing is missing: the draft can be sent once you have checked it.</Text>
          )}
        </VStack>
      ),
      validate: () => {
        const problem = firstProblem();
        if (!problem) return null;
        setShowErrors({ customer: true, parties: true, plan: true, term: true, legal: true });
        navRef.current?.goTo(problem.step);
        return problem.message;
      },
    },
  ];

  const error = fe.summary ? <Banner status="error" title="The Order Form was not created" description={fe.summary} /> : null;

  return <Wizard label="New Order Form" steps={steps} finishLabel="Create draft" onFinish={create} isFinishing={m.isPending} isDirty={dirty} error={error} />;
}
