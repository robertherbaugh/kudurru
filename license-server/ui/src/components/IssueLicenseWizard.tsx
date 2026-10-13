// "Issue license" (POST /api/v1/licenses) in a WizardDialog over the page
// the admin is on (docs/m3/ui.md: wizards only for creation), in four
// steps:
// 1. Customer: an existing one (CustomerField) or a new one created inline
//    (a wizard never opens another dialog);
// 2. Plan and limits: Personal or Business (Enterprise and Trial behind
//    "More plans" with their caveat), the licensee printed on the key, the
//    node limit and the features (never an empty explicit list);
// 3. Term: the expiry day (UTC) with presets, the agreement reference and
//    notes, and whether to mail the key;
// 4. Review: every claim in words with Expires (UTC) and Keeps working
//    until (UTC), then "Sign and issue" (which checks every step again:
//    the Stepper lets the admin change an earlier step and jump back).
// Signing closes the dialog and opens the new license's page. A 400 that
// names fields takes the admin to the first step they belong to and shows
// the server's message under the control (api/fieldErrors).

import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { MetadataList } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useRef, useState } from 'react';
import type { CustomerKind, IssueLicenseBody } from '@shared/api';
import { PLANS, planLabel, PERSONAL_MAX_NODES, type Feature, type Plan } from '@shared/catalog';
import { graceEndsOn, todayUtc } from '@shared/dates';
import { firstField, useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useRouteMutation } from '../api/hooks';
import { WizardNavHandle, type WizardNav } from './WizardNavHandle';
import { utcDay } from '../lib/dates';
import { defaultExpiry, termBounds, termPresets } from '../lib/terms';
import { emailError, maxNodesFor, nameError, nodesError } from '../lib/validate';
import { KIND_LABEL, KIND_OPTIONS } from '../lib/words';
import { useNotify } from '../ui/components/Feedback';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../ui/components/Wizard';
import { ChoiceCards } from '../ui/fields/ChoiceCards';
import { CustomerField, type CustomerChoice } from '../ui/fields/CustomerField';
import { DateField, dateError } from '../ui/fields/DateField';
import { featuresError, FeaturesField, featuresInWords } from '../ui/fields/FeaturesField';
import { NumberField } from '../ui/fields/NumberField';
import { SelectField } from '../ui/fields/SelectField';
import { CheckboxField, TextAreaField, TextField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';
import { useNavigate } from '../ui/lib/router';

export interface IssueLicenseWizardProps {
  isOpen: boolean;
  onClose: () => void;
  /** Opened from a customer's page: that customer, chosen. */
  customer?: CustomerChoice | undefined;
}

export function IssueLicenseWizardDialog({ isOpen, onClose, customer }: IssueLicenseWizardProps) {
  return (
    <WizardDialog isOpen={isOpen} onClose={onClose} title="Issue license" subtitle="Signs a key for a customer." what="license">
      <IssueLicenseWizard onClose={onClose} customer={customer} />
    </WizardDialog>
  );
}

const MORE_PLANS: readonly Plan[] = ['enterprise', 'trial'];

/** The caveat of the plans no public terms describe yet (critique product-ux#13). */
export const MORE_PLANS_CAVEAT = 'Enterprise and Trial appear in no public terms yet; issue them only under a written agreement. Any key keeps working for 30 days after it expires, a trial too.';

/** The body fields the steps' controls show, in step order. */
const ISSUE_FIELDS = ['customerId', 'newCustomer.name', 'newCustomer.kind', 'newCustomer.contactEmail', 'licensee', 'plan', 'nodes', 'features', 'expiresOn', 'agreementRef', 'notes', 'emailCustomer'];

/** The step whose control shows a body field. */
function stepOfField(field: string): string {
  if (field === 'customerId' || field.startsWith('newCustomer')) return 'customer';
  if (['licensee', 'plan', 'nodes', 'features'].includes(field)) return 'limits';
  return 'term';
}

function IssueLicenseWizard({ onClose, customer: preset }: { onClose: () => void; customer?: CustomerChoice | undefined }) {
  const catalog = useCatalog();
  const notify = useNotify();
  const navigate = useNavigate();
  const m = useRouteMutation('licenseIssue');
  const fe = useFieldErrors(m.error, ISSUE_FIELDS);
  const navRef = useRef<WizardNav | null>(null);
  const today = catalog.data?.today ?? todayUtc();
  const mailConfigured = catalog.data?.mail.configured ?? false;

  const [mode, setMode] = useState<'existing' | 'new'>('existing');
  const [customer, setCustomer] = useState<CustomerChoice | null>(preset ?? null);
  const [newName, setNewName] = useState('');
  const [newKind, setNewKind] = useState<CustomerKind>('organization');
  const [newEmail, setNewEmail] = useState('');
  const [plan, setPlan] = useState<Plan>('business');
  const [showMore, setShowMore] = useState(false);
  const [licensee, setLicensee] = useState(preset?.name ?? '');
  const [licenseeEdited, setLicenseeEdited] = useState(false);
  const [nodes, setNodes] = useState('1');
  const [features, setFeatures] = useState<Feature[] | null>(null);
  const [expiresOn, setExpiresOn] = useState(() => defaultExpiry('business', today));
  const [agreementRef, setAgreementRef] = useState('');
  const [notes, setNotes] = useState('');
  const [emailCustomer, setEmailCustomer] = useState(false);
  const [showErrors, setShowErrors] = useState<Record<string, boolean>>({});
  const [dirty, setDirty] = useState(false);

  const customerName = mode === 'existing' ? (customer?.name ?? '') : newName;
  const contactEmail = mode === 'existing' ? (customer?.contactEmail ?? null) : newEmail.trim() || null;
  const bounds = termBounds(plan, today);
  const presets = termPresets(plan, today, bounds.max);
  const plans = PLANS.filter((p) => showMore || !MORE_PLANS.includes(p.key) || p.key === plan);

  // The licensee follows the customer's name until the admin types one.
  const followName = (name: string) => {
    if (!licenseeEdited) setLicensee(name);
  };

  const choosePlan = (v: string) => {
    const p = v as Plan;
    setPlan(p);
    setDirty(true);
    fe.edited('plan');
    if (p === 'personal') {
      setNodes(String(PERSONAL_MAX_NODES));
      setFeatures(null);
    } else if (Number(nodes) > maxNodesFor(p)) setNodes(String(maxNodesFor(p)));
    const b = termBounds(p, today);
    if (dateError(expiresOn, b)) setExpiresOn(defaultExpiry(p, today));
  };

  const customerError = (): string | null => {
    if (mode === 'existing') return customer ? null : 'Choose a customer, or create a new one';
    return nameError(newName, 'the customer name') ?? emailError(newEmail, { optional: true });
  };
  const limitsError = (): string | null => nameError(licensee, 'the licensee') ?? nodesError(nodes, plan) ?? featuresError(features);
  const termError = (): string | null => dateError(expiresOn, bounds);

  /** Every step's check, for the review: the first failing step and its message. */
  const firstProblem = (): { step: string; message: string } | null => {
    const checks: [string, () => string | null][] = [
      ['customer', customerError],
      ['limits', limitsError],
      ['term', termError],
    ];
    for (const [step, check] of checks) {
      const message = check();
      if (message) return { step, message };
    }
    return null;
  };

  const issue = async () => {
    // The review step's validate already refused this; never send a half-made body.
    if (firstProblem() || (mode === 'existing' && !customer)) return;
    const fields = {
      licensee: licensee.trim(),
      plan,
      nodes: Number(nodes),
      features,
      expiresOn,
      ...(agreementRef.trim() ? { agreementRef: agreementRef.trim() } : {}),
      ...(notes.trim() ? { notes: notes.trim() } : {}),
      ...(emailCustomer && contactEmail ? { emailCustomer: true } : {}),
    };
    // Never send an empty explicit list (spec §0.3); the steps' checks already refused it.
    if (featuresError(fields.features)) return;
    const body: IssueLicenseBody =
      mode === 'existing' && customer
        ? { ...fields, customerId: customer.id }
        : { ...fields, newCustomer: { name: newName.trim(), kind: newKind, contactEmail: newEmail.trim() || null } };
    try {
      const d = await m.mutateAsync({ body });
      notify.success(`Issued ${d.id} to ${d.licensee}`);
      onClose();
      navigate(`/licenses/${encodeURIComponent(d.id)}`);
    } catch (err) {
      // A refusal that names fields: to the first step they belong to, the message under the control.
      const field = firstField(err, ISSUE_FIELDS);
      if (field) navRef.current?.goTo(stepOfField(field));
      // Anything else is shown above the footer (fe.summary).
    }
  };

  const steps: WizardStep[] = [
    {
      id: 'customer',
      label: 'Customer',
      description: 'Who the license is for. Its members see the license and its key in the portal.',
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
              followName(v === 'existing' ? (customer?.name ?? '') : newName);
            }}
            choices={[
              { value: 'existing', label: 'Existing customer', description: 'Search by name, contact email or member email.' },
              { value: 'new', label: 'New customer', description: 'Created together with the license.' },
            ]}
          />
          {mode === 'existing' ? (
            <CustomerField
              value={customer}
              onChange={(c) => {
                setCustomer(c);
                setDirty(true);
                fe.edited('customerId');
                followName(c?.name ?? '');
              }}
              isRequired
              error={(showErrors.customer && !customer ? 'Choose a customer' : null) ?? fe.field('customerId')}
            />
          ) : (
            <VStack gap={3}>
              <TextField
                label="Customer name"
                value={newName}
                onChange={(v) => {
                  setNewName(v);
                  fe.edited('newCustomer.name');
                  followName(v);
                }}
                isRequired
                error={(showErrors.customer ? nameError(newName, 'the customer name') : null) ?? fe.field('newCustomer.name')}
              />
              <SelectField
                label="Kind"
                value={newKind}
                onChange={(v) => {
                  setNewKind(v as CustomerKind);
                  fe.edited('newCustomer.kind');
                }}
                options={KIND_OPTIONS}
                error={fe.field('newCustomer.kind')}
              />
              <TextField
                label="Contact email"
                type="email"
                value={newEmail}
                onChange={(v) => {
                  setNewEmail(v);
                  fe.edited('newCustomer.contactEmail');
                }}
                isOptional
                help="Where keys are mailed when you choose to. Members are added on the customer's page."
                error={(showErrors.customer ? emailError(newEmail, { optional: true }) : null) ?? fe.field('newCustomer.contactEmail')}
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
    {
      id: 'limits',
      label: 'Plan and limits',
      description: 'What the key says: the plan, the licensee printed on it, the node limit and the features.',
      content: (
        <VStack gap={4}>
          <VStack gap={1}>
            <ChoiceCards
              label="Plan"
              value={plan}
              onChange={choosePlan}
              choices={plans.map((p) => ({ value: p.key, label: p.label, description: p.description }))}
              help={showMore ? MORE_PLANS_CAVEAT : undefined}
              error={fe.field('plan')}
            />
            {!MORE_PLANS.includes(plan) ? (
              <Button label={showMore ? 'Fewer plans' : 'More plans'} variant="ghost" size="sm" onClick={() => setShowMore((s) => !s)} />
            ) : null}
          </VStack>
          <TextField
            label="Licensee"
            value={licensee}
            onChange={(v) => {
              setLicensee(v);
              setLicenseeEdited(true);
              fe.edited('licensee');
            }}
            isRequired
            help="Printed on the key and shown on every node that runs it. A name, never an email address."
            error={(showErrors.limits ? nameError(licensee, 'the licensee') : null) ?? fe.field('licensee')}
          />
          <NumberField
            label="Nodes"
            value={nodes}
            onChange={(v) => {
              setNodes(v);
              fe.edited('nodes');
            }}
            min={1}
            max={maxNodesFor(plan)}
            isDisabled={plan === 'personal'}
            help={plan === 'personal' ? `A personal key always covers ${PERSONAL_MAX_NODES} nodes.` : 'The most nodes one cluster may run with this key.'}
            error={fe.field('nodes')}
          />
          <FeaturesField
            value={features}
            onChange={(v) => {
              setFeatures(v);
              setDirty(true);
              fe.edited('features');
            }}
            showErrors={showErrors.limits}
            lockedAllReason={plan === 'personal' ? 'A personal key always carries every feature.' : undefined}
            error={fe.field('features')}
          />
        </VStack>
      ),
      validate: () => {
        setShowErrors((s) => ({ ...s, limits: true }));
        return limitsError();
      },
    },
    {
      id: 'term',
      label: 'Term',
      description: 'The last day the key is valid, in UTC. Nodes keep accepting it for 30 days after that.',
      content: (
        <VStack gap={4}>
          <DateField
            label="Expires on (UTC)"
            value={expiresOn}
            onChange={(v) => {
              setExpiresOn(v);
              setDirty(true);
              fe.edited('expiresOn');
            }}
            min={bounds.min}
            max={bounds.max}
            presets={presets}
            isRequired
            help={expiresOn && !termError() ? `Keeps working on nodes until ${utcDay(graceEndsOn(expiresOn))}.` : `Between ${utcDay(bounds.min)} and ${utcDay(bounds.max)}.`}
            error={(showErrors.term ? termError() : null) ?? fe.field('expiresOn')}
          />
          {plan !== 'personal' ? (
            <TextField
              label="Agreement reference"
              value={agreementRef}
              onChange={(v) => {
                setAgreementRef(v);
                fe.edited('agreementRef');
              }}
              isOptional
              help="The contract or order this license records, for your own records. Not on the key."
              error={fe.field('agreementRef')}
            />
          ) : null}
          <TextAreaField
            label="Notes"
            value={notes}
            onChange={(v) => {
              setNotes(v);
              fe.edited('notes');
            }}
            isOptional
            help="Internal; customers never see them."
            error={fe.field('notes')}
          />
          {mailConfigured ? (
            <CheckboxField
              label="Mail the key to the customer's contact email"
              value={emailCustomer && Boolean(contactEmail)}
              onChange={(v) => {
                setEmailCustomer(v);
                fe.edited('emailCustomer');
              }}
              isDisabled={!contactEmail}
              help={contactEmail ? `Sent to ${contactEmail} after signing.` : 'The customer has no contact email.'}
              error={fe.field('emailCustomer')}
            />
          ) : null}
        </VStack>
      ),
      validate: () => {
        setShowErrors((s) => ({ ...s, term: true }));
        return termError();
      },
    },
    {
      id: 'review',
      label: 'Review',
      description: 'The key says exactly this once it is signed. A signed key cannot be recalled: withdrawing a license only stops offering and renewing it.',
      content: (
        <VStack gap={3}>
          <WizardNavHandle navRef={navRef} />
          <MetadataList>
            <ReviewRow label="Customer" step="customer" what="customer">
              {mode === 'existing' ? customerName : `${customerName} (new ${KIND_LABEL[newKind].toLowerCase()}${newEmail.trim() ? `, ${newEmail.trim()}` : ''})`}
            </ReviewRow>
            <ReviewRow label="Licensee" step="limits" what="licensee">
              {licensee.trim()}
            </ReviewRow>
            <ReviewRow label="Plan" step="limits" what="plan">
              {planLabel(plan)}
            </ReviewRow>
            <ReviewRow label="Nodes" step="limits" what="nodes">
              {formatExact(Number(nodes))}
            </ReviewRow>
            <ReviewRow label="Features" step="limits" what="features">
              {featuresInWords(features)}
            </ReviewRow>
            <ReviewRow label="Expires (UTC)" step="term" what="expiry">
              <Text type="inherit" data-testid="review-expires">
                {utcDay(expiresOn)}
              </Text>
            </ReviewRow>
            <ReviewRow label="Keeps working until (UTC)" step="term" what="expiry">
              <Text type="inherit" data-testid="review-keeps-working">
                {termError() ? '–' : utcDay(graceEndsOn(expiresOn))}
              </Text>
            </ReviewRow>
            {agreementRef.trim() ? (
              <ReviewRow label="Agreement reference" step="term" what="agreement reference">
                {agreementRef.trim()}
              </ReviewRow>
            ) : null}
            {notes.trim() ? (
              <ReviewRow label="Notes" step="term" what="notes">
                {notes.trim()}
              </ReviewRow>
            ) : null}
            <ReviewRow label="Mail the key" step="term" what="mail">
              {emailCustomer && contactEmail ? `Yes, to ${contactEmail}` : 'No'}
            </ReviewRow>
          </MetadataList>
          {plan === 'personal' ? <Text type="supporting">The key records an admin attestation of non-commercial use, by you, now.</Text> : null}
        </VStack>
      ),
      // The Stepper lets the admin change an earlier step and come back: check them all again.
      validate: () => {
        const problem = firstProblem();
        if (!problem) return null;
        setShowErrors({ customer: true, limits: true, term: true });
        navRef.current?.goTo(problem.step);
        return problem.message;
      },
    },
  ];

  const error = fe.summary ? <Banner status="error" title="The license was not issued" description={fe.summary} /> : null;

  return <Wizard label="Issue license" steps={steps} finishLabel="Sign and issue" onFinish={issue} isFinishing={m.isPending} isDirty={dirty} error={error} />;
}
