// The terms of an Order Form, one section at a time (lib/orderForms
// SECTION_FIELDS): the typed controls the wizard's steps and the detail
// page's inline sections share (TermsFields), and the read-only rows the
// admin's and the customer's pages show (TermsView). Fees are typed in US
// dollars with the price list's amounts in the help (shared/pricing),
// "Use the price list", and a note when the fees differ from the list or
// the node count needs an individual quote.
import { Button } from '@astryxdesign/core/Button';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { ReactNode } from 'react';
import type { OrderFormTerms } from '@shared/api';
import { BUSINESS_EDITIONS, FEATURES, LICENSOR_NAME, MAX_NODES, ORDER_FORM_MAX_CONTACTS, type BusinessEdition } from '@shared/catalog';
import { addDays } from '@shared/dates';
import { editionLabel, feesInWords, modulesInWords, ORDER_FORM_DEFAULTS } from '@shared/orderForms';
import { listPrice, usd, VOLUME_PRICING } from '@shared/pricing';
import { utcDay } from '../lib/dates';
import { dollarsToCents, draftErrors, listFees, maxSubscriptionEnd, plainWords, TERM_LABEL, type TermsDraft, type TermsField, type TermsSection } from '../lib/orderForms';
import { emailError } from '../lib/validate';
import { ChoiceCards } from '../ui/fields/ChoiceCards';
import { DateField } from '../ui/fields/DateField';
import { NumberField } from '../ui/fields/NumberField';
import { SelectField } from '../ui/fields/SelectField';
import { StringListField } from '../ui/fields/StringListField';
import { TextAreaField, TextField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';

export interface TermsFieldsProps {
  section: TermsSection;
  draft: TermsDraft;
  onChange: (patch: Partial<TermsDraft>) => void;
  /** Show the local checks' messages (after a failed Next or Save). */
  showErrors: boolean;
  /** The server's message for a body path ("terms.nodes"). */
  serverError: (path: string) => string | undefined;
  /** A control was edited: its server message goes away. */
  edited: (path: string) => void;
}

/** The typed controls of one section of the terms. */
export function TermsFields({ section, draft: d, onChange, showErrors, serverError, edited }: TermsFieldsProps) {
  const local = draftErrors(d);
  const err = (f: TermsField): string | undefined => (showErrors ? local[f] : undefined) ?? serverError(`terms.${f}`);
  const set = (f: TermsField, patch: Partial<TermsDraft>) => {
    onChange(patch);
    edited(`terms.${f}`);
  };
  const text = (f: TermsField & keyof TermsDraft, opts: { multiline?: boolean; help?: ReactNode; isOptional?: boolean; placeholder?: string } = {}) => {
    const Field = opts.multiline ? TextAreaField : TextField;
    return (
      <Field
        label={TERM_LABEL[f]}
        value={d[f] as string}
        onChange={(v: string) => set(f, { [f]: v })}
        isOptional={opts.isOptional}
        placeholder={opts.placeholder}
        help={opts.help}
        error={err(f)}
      />
    );
  };

  switch (section) {
    case 'parties':
      return (
        <VStack gap={3}>
          {text('customerLegalName', { help: 'The name that binds, as in the commercial register.' })}
          {text('customerAddress', { multiline: true, help: 'The registered address.' })}
          {text('billingContact', { multiline: true, help: 'Who receives invoices: a name and an address.' })}
          <StringListField
            label={TERM_LABEL.authorizedContacts}
            value={d.authorizedContacts}
            onChange={(v) => set('authorizedContacts', { authorizedContacts: v })}
            type="email"
            placeholder="name@example.com"
            itemError={(s) => emailError(s)}
            addLabel="Add contact"
            maxItems={ORDER_FORM_MAX_CONTACTS}
            description={`The people who may accept the form for the customer, 1 to ${ORDER_FORM_MAX_CONTACTS}. Sending makes each a member of the customer and mails them a link to review it.`}
            error={err('authorizedContacts')}
          />
          {text('licensorContact', {
            multiline: true,
            isOptional: true,
            help: `The licensor is ${LICENSOR_NAME}; this is the address and billing contact printed under the name. Left empty on a new form, the address of the last Order Form is used.`,
          })}
        </VStack>
      );
    case 'plan':
      return <PlanFields d={d} set={set} err={err} text={text} />;
    case 'term':
      return <TermFields d={d} onChange={onChange} edited={edited} set={set} err={err} text={text} />;
    case 'legal':
      return (
        <VStack gap={3}>
          {text('keyPolicy', { multiline: true, help: 'Key delivery, offline use, deployment id.' })}
          {text('support', { multiline: true, help: `"${ORDER_FORM_DEFAULTS.support}" when none is included.` })}
          {text('renewal', { multiline: true, help: 'The agreement never renews on its own unless this says so.' })}
          {text('governingLaw', { multiline: true, help: 'The law that governs, and the courts and venue.' })}
          {text('specialTerms', { multiline: true, isOptional: true, help: 'Express overrides of the agreement; empty for none.' })}
        </VStack>
      );
  }
}

type Setter = (f: TermsField, patch: Partial<TermsDraft>) => void;
type ErrorOf = (f: TermsField) => string | undefined;
type TextOf = (f: TermsField & keyof TermsDraft, opts?: { multiline?: boolean; help?: ReactNode; isOptional?: boolean; placeholder?: string }) => ReactNode;

const EDITION_HELP: Record<BusinessEdition, string> = {
  core: 'Issued as a Business key. Enterprise modules only when named.',
  enterprise: 'Issued as an Enterprise key, with all current modules or named ones.',
};

function PlanFields({ d, set, err, text }: { d: TermsDraft; set: Setter; err: ErrorOf; text: TextOf }) {
  return (
    <VStack gap={3}>
      <ChoiceCards
        label={TERM_LABEL.edition}
        value={d.edition}
        onChange={(v) => set('edition', { edition: v as BusinessEdition })}
        choices={(Object.keys(BUSINESS_EDITIONS) as BusinessEdition[]).map((e) => ({ value: e, label: editionLabel(e), description: EDITION_HELP[e] }))}
        error={err('edition')}
      />
      <ChoiceCards
        label={TERM_LABEL.modules}
        value={d.modules}
        onChange={(v) => set('modules', { modules: v as TermsDraft['modules'] })}
        choices={[
          { value: 'all', label: 'All current modules', description: 'Every `ee/` module; the key carries every feature.', isDisabled: d.edition === 'core' },
          { value: 'named', label: 'Named modules', description: 'Only the modules named below.' },
          { value: 'none', label: 'None', description: 'No Enterprise module.', isDisabled: d.edition === 'enterprise' },
        ]}
        help={d.edition === 'core' ? 'All current modules is the Enterprise plan.' : 'An Enterprise plan includes all current modules or named ones.'}
        error={err('modules')}
      />
      {d.modules === 'named' ? (
        <ChoiceCards
          label={TERM_LABEL.namedModules}
          multiple
          hasDividers
          value={[...d.namedModules]}
          onChange={(v) => set('namedModules', { namedModules: FEATURES.filter((f) => v.includes(f.key)).map((f) => f.key) })}
          choices={FEATURES.map((f) => ({ value: f.key, label: f.label, description: f.unlocks }))}
          error={err('namedModules')}
        />
      ) : null}
      <NumberField
        label={TERM_LABEL.nodes}
        value={d.nodes}
        onChange={(v) => set('nodes', { nodes: v })}
        min={1}
        max={MAX_NODES}
        help="Concurrent running production nodes."
        error={err('nodes')}
      />
      {text('environments', { help: 'Where the nodes run, e.g. "all covered environments and clusters".' })}
      {text('permittedScope', { multiline: true })}
      <NumberField
        label={TERM_LABEL.nonproductionNodes}
        value={d.nonproductionNodes}
        onChange={(v) => set('nonproductionNodes', { nonproductionNodes: v })}
        min={0}
        max={MAX_NODES}
        help="Running nonproduction and standby nodes on top of the production nodes; 0 for none (the agreement's default)."
        error={err('nonproductionNodes')}
      />
      {text('nonproductionTerms', { multiline: true, help: 'Which nonproduction nodes, and whether running disaster recovery counts.' })}
      <SelectField
        label={TERM_LABEL.serviceProvider}
        value={d.serviceProvider}
        onChange={(v) => set('serviceProvider', { serviceProvider: v as TermsDraft['serviceProvider'] })}
        options={[
          { value: 'not_authorized', label: 'Not authorized', help: 'The customer runs kudurru for itself only.' },
          { value: 'authorized', label: 'Authorized', help: 'The customer may run kudurru for the clients named in the scope.' },
        ]}
        error={err('serviceProvider')}
      />
      {d.serviceProvider === 'authorized' ? text('serviceProviderScope', { multiline: true, help: 'The clients, or the pooled scope.' }) : null}
      <SelectField
        label={TERM_LABEL.government}
        value={d.government}
        onChange={(v) => set('government', { government: v as TermsDraft['government'] })}
        options={[
          { value: 'no', label: 'No' },
          { value: 'yes', label: 'Yes', help: 'Name the agencies and the procurement addendum in the scope.' },
        ]}
        error={err('government')}
      />
      {d.government === 'yes' ? text('governmentScope', { multiline: true, help: 'The agencies and the procurement addendum.' }) : null}
    </VStack>
  );
}

function TermFields({ d, onChange, edited, set, err, text }: { d: TermsDraft; onChange: (p: Partial<TermsDraft>) => void; edited: (path: string) => void; set: Setter; err: ErrorOf; text: TextOf }) {
  const nodes = Number(d.nodes.trim() || 'NaN');
  const price = listPrice(d.edition, nodes);
  const list = listFees(d);
  const quote = Number.isSafeInteger(nodes) && nodes >= VOLUME_PRICING.quoteFrom;
  const annual = dollarsToCents(d.annualFee);
  const platform = dollarsToCents(d.platformFee);
  const perNode = dollarsToCents(d.perNodeFee);
  const composed = platform !== null && perNode !== null && Number.isSafeInteger(nodes) ? platform + perNode * nodes : null;
  const differs = price && annual !== null && annual !== price.annualCents ? `Differs from the price list (${usd(price.annualCents)}).` : undefined;
  const quoteNote = quote ? `${formatExact(nodes)} nodes need an individual quote (${VOLUME_PRICING.quoteFrom} or more): enter the agreed fees.` : undefined;
  const maxEnd = maxSubscriptionEnd(d.subscriptionStart);
  const minEnd = !err('subscriptionStart') && d.subscriptionStart ? addDays(d.subscriptionStart, 1) : undefined;
  return (
    <VStack gap={3}>
      <HStack gap={3} wrap="wrap" vAlign="start">
        <DateField
          label={TERM_LABEL.subscriptionStart}
          value={d.subscriptionStart}
          onChange={(v) => set('subscriptionStart', { subscriptionStart: v })}
          isRequired
          help="UTC."
          error={err('subscriptionStart')}
        />
        <DateField
          label={TERM_LABEL.subscriptionEnd}
          value={d.subscriptionEnd}
          onChange={(v) => set('subscriptionEnd', { subscriptionEnd: v })}
          min={minEnd}
          max={maxEnd}
          isRequired
          help="The last day, UTC; at most ten years after the start. A key issued from the form expires on this day."
          error={err('subscriptionEnd')}
        />
        <DateField
          label={TERM_LABEL.effectiveDate}
          value={d.effectiveDate}
          onChange={(v) => set('effectiveDate', { effectiveDate: v })}
          max={d.subscriptionEnd || undefined}
          isOptional
          help="Empty: the subscription start."
          error={err('effectiveDate')}
        />
      </HStack>
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Button
          label="Use the price list"
          size="sm"
          variant="secondary"
          isDisabled={!list}
          tooltip={list ? undefined : 'No list price for this node count'}
          onClick={() => {
            if (!list) return;
            onChange(list);
            for (const f of ['platformFeeCents', 'perNodeFeeCents', 'annualFeeCents']) edited(`terms.${f}`);
          }}
        />
        <Text type="supporting">
          {price
            ? `Price list for ${editionLabel(d.edition)}, ${formatExact(nodes)} ${nodes === 1 ? 'node' : 'nodes'}: ${usd(price.annualCents)} a year.`
            : (quoteNote ?? 'Enter the production nodes to see the price list.')}
        </Text>
      </HStack>
      <TextField
        label={TERM_LABEL.platformFeeCents}
        value={d.platformFee}
        onChange={(v) => set('platformFeeCents', { platformFee: v })}
        placeholder="0.00"
        isRequired
        help={price ? `US dollars a year. Price list: ${usd(price.platformCents)}.` : 'US dollars a year.'}
        error={err('platformFeeCents')}
      />
      <TextField
        label={TERM_LABEL.perNodeFeeCents}
        value={d.perNodeFee}
        onChange={(v) => set('perNodeFeeCents', { perNodeFee: v })}
        placeholder="0.00"
        isRequired
        help={
          price
            ? `US dollars per node a year. Price list: ${usd(price.perNodeCents)}${price.discounted ? ` (${Math.round(VOLUME_PRICING.discount * 100)}% off from ${VOLUME_PRICING.discountFrom} nodes)` : ''}.`
            : 'US dollars per node a year.'
        }
        error={err('perNodeFeeCents')}
      />
      <TextField
        label={TERM_LABEL.annualFeeCents}
        value={d.annualFee}
        onChange={(v) => set('annualFeeCents', { annualFee: v })}
        placeholder="0.00"
        isRequired
        help={`What the customer pays a year, in US dollars.${composed !== null && annual !== null && composed !== annual ? ` Platform fee + nodes × Node fee is ${usd(composed)}.` : ''}`}
        warning={differs ?? quoteNote}
        error={err('annualFeeCents')}
      />
      {text('paymentTerms', { multiline: true, help: 'Invoicing and the payment schedule.' })}
    </VStack>
  );
}

function Lines({ text }: { text: string }) {
  const lines = text.trim().split(/\r?\n/).filter((l) => l.trim() !== '');
  if (!lines.length) return <Text color="secondary">Not set</Text>;
  return (
    <VStack gap={0.5}>
      {lines.map((l, i) => (
        <Text key={i}>{l}</Text>
      ))}
    </VStack>
  );
}

/** The read-only rows of one section of the terms. */
export function TermsView({ section, terms: t }: { section: TermsSection; terms: OrderFormTerms }) {
  switch (section) {
    case 'parties':
      return (
        <MetadataList>
          <MetadataListItem label={TERM_LABEL.customerLegalName}>
            <Lines text={t.customerLegalName} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.customerAddress}>
            <Lines text={t.customerAddress} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.billingContact}>
            <Lines text={t.billingContact} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.authorizedContacts}>
            <Lines text={t.authorizedContacts.join('\n')} />
          </MetadataListItem>
          <MetadataListItem label="Licensor">
            <VStack gap={0.5}>
              <Text>{LICENSOR_NAME}</Text>
              {t.licensorContact.trim() ? <Lines text={t.licensorContact} /> : <Text color="secondary">Address not set</Text>}
            </VStack>
          </MetadataListItem>
        </MetadataList>
      );
    case 'plan':
      return (
        <MetadataList>
          <MetadataListItem label={TERM_LABEL.edition}>{editionLabel(t.edition)}</MetadataListItem>
          <MetadataListItem label={TERM_LABEL.modules}>{plainWords(modulesInWords(t))}</MetadataListItem>
          <MetadataListItem label={TERM_LABEL.nodes}>
            {formatExact(t.nodes)} concurrent running {t.nodes === 1 ? 'node' : 'nodes'} across {t.environments.trim() || '–'}
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.permittedScope}>
            <Lines text={t.permittedScope} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.nonproductionNodes}>
            <VStack gap={0.5}>
              <Text>{formatExact(t.nonproductionNodes)}</Text>
              {t.nonproductionTerms.trim() ? <Lines text={t.nonproductionTerms} /> : null}
            </VStack>
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.serviceProvider}>{t.serviceProvider === 'authorized' ? `Authorized: ${t.serviceProviderScope.trim() || 'scope not set'}` : 'Not authorized'}</MetadataListItem>
          <MetadataListItem label={TERM_LABEL.government}>{t.government === 'yes' ? `Yes: ${t.governmentScope.trim() || 'scope not set'}` : 'No'}</MetadataListItem>
        </MetadataList>
      );
    case 'term':
      return (
        <MetadataList>
          <MetadataListItem label="Subscription">
            {utcDay(t.subscriptionStart)} to {utcDay(t.subscriptionEnd)}
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.effectiveDate}>{t.effectiveDate ? utcDay(t.effectiveDate) : `${utcDay(t.subscriptionStart)} (the subscription start)`}</MetadataListItem>
          <MetadataListItem label="Fees">{feesInWords(t)}</MetadataListItem>
          <MetadataListItem label={TERM_LABEL.paymentTerms}>
            <Lines text={t.paymentTerms} />
          </MetadataListItem>
        </MetadataList>
      );
    case 'legal':
      return (
        <MetadataList>
          <MetadataListItem label={TERM_LABEL.keyPolicy}>
            <Lines text={t.keyPolicy} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.support}>
            <Lines text={t.support} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.renewal}>
            <Lines text={t.renewal} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.governingLaw}>
            <Lines text={t.governingLaw} />
          </MetadataListItem>
          <MetadataListItem label={TERM_LABEL.specialTerms}>{t.specialTerms.trim() ? <Lines text={t.specialTerms} /> : 'None'}</MetadataListItem>
        </MetadataList>
      );
  }
}
