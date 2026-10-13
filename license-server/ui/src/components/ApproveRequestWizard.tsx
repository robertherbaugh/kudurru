// "Approve" a personal key request (POST /api/v1/requests/:id/approve) in
// a WizardDialog, in three steps:
// 1. Customer: by default the person customer the portal already has for
//    the requester's email (LicenseRequest.personCustomer, which the server
//    would use anyway), else a new person customer made from the request;
//    or another existing person customer (never an organization: approving
//    never adds the requester to one);
// 2. Key: the licensee printed on the key and its expiry (UTC). For a
//    renewal (LicenseRequest.renews, with the default customer) only the
//    expiry: the renewed key keeps its licensee, and the proposed expiry is
//    one term after the later of today and the current expiry, so the
//    server's "a renewal must expire later" rule holds;
// 3. Review: every claim in words, the use the requester described and
//    their certification, then "Approve and send key" (which checks the
//    earlier steps again).
// The server issues (or renews, if the person holds a personal license)
// and mails the key. A 400 that names fields takes the admin to the step
// of the first one, with the message under the control.
import { Banner } from '@astryxdesign/core/Banner';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useRef, useState } from 'react';
import type { ApproveBody, LicenseRequest } from '@shared/api';
import { ALL_FEATURES_LABEL, PERSONAL_MAX_NODES, PERSONAL_TERM_DAYS, planLabel } from '@shared/catalog';
import { addDays, graceEndsOn, maxExpiresOn, todayUtc, type ExpiresOn } from '@shared/dates';
import { firstField, useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useRouteMutation } from '../api/hooks';
import { utcDateTime, utcDay } from '../lib/dates';
import { defaultExpiry, renewalBase, termBounds, termPresets } from '../lib/terms';
import { nameError } from '../lib/validate';
import { useNotify } from '../ui/components/Feedback';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../ui/components/Wizard';
import { WizardNavHandle, type WizardNav } from './WizardNavHandle';
import { ChoiceCards } from '../ui/fields/ChoiceCards';
import { CustomerField, type CustomerChoice } from '../ui/fields/CustomerField';
import { DateField, dateError } from '../ui/fields/DateField';
import { TextField } from '../ui/fields/TextField';

export function ApproveRequestWizardDialog({ request, isOpen, onClose }: { request: LicenseRequest | null; isOpen: boolean; onClose: () => void }) {
  return (
    <WizardDialog isOpen={isOpen && request !== null} onClose={onClose} title="Approve request" subtitle={request ? `${request.name} · ${request.requesterEmail}` : undefined} what="approval">
      {request ? <ApproveWizard request={request} onClose={onClose} /> : null}
    </WizardDialog>
  );
}

/** The body fields the steps' controls show, in step order. */
const APPROVE_FIELDS = ['customerId', 'licensee', 'expiresOn'];

type Renews = NonNullable<LicenseRequest['renews']>;

/** The expiry bounds of the key approving signs: a renewal must also expire after the current key. */
export function approvalBounds(today: ExpiresOn, renews: Renews | null): { min: ExpiresOn; max: ExpiresOn } {
  const bounds = termBounds('personal', today);
  return renews ? { ...bounds, min: maxExpiresOn(bounds.min, addDays(renews.expiresOn, 1)) } : bounds;
}

/** The proposed expiry: one term from today, or for a renewal one term after the later of today and the current expiry (within the bounds). */
export function approvalDefaultExpiry(today: ExpiresOn, renews: Renews | null): ExpiresOn {
  if (!renews) return defaultExpiry('personal', today);
  const proposed = addDays(renewalBase(today, renews.expiresOn), PERSONAL_TERM_DAYS);
  const { max } = termBounds('personal', today);
  return proposed <= max ? proposed : max;
}

function ApproveWizard({ request: r, onClose }: { request: LicenseRequest; onClose: () => void }) {
  const catalog = useCatalog();
  const notify = useNotify();
  const m = useRouteMutation('requestApprove');
  const fe = useFieldErrors(m.error, APPROVE_FIELDS);
  const navRef = useRef<WizardNav | null>(null);
  const today = catalog.data?.today ?? todayUtc();
  const known = r.personCustomer;
  // 'default': the person customer the portal has for the email (known), else a new one; 'other': one the admin picks.
  const [mode, setMode] = useState<'default' | 'other'>('default');
  const [customer, setCustomer] = useState<CustomerChoice | null>(null);
  const [licensee, setLicensee] = useState(r.name);
  const [expiresOn, setExpiresOn] = useState(() => approvalDefaultExpiry(today, r.renews));
  const [showErrors, setShowErrors] = useState<Record<string, boolean>>({});
  // What approving renews is known for the default customer only; another customer may hold another license (the server decides).
  const renews = mode === 'default' ? r.renews : null;
  const bounds = approvalBounds(today, renews);

  const customerError = (): string | null => (mode === 'other' && !customer ? 'Choose a person customer' : null);
  const keyError = (): string | null => (renews ? null : nameError(licensee, 'the licensee')) ?? dateError(expiresOn, bounds);
  const customerId = mode === 'other' ? customer?.id : known?.id;

  const approve = async () => {
    if (customerError() ?? keyError()) return;
    // A renewal keeps the license's licensee: the server ignores one sent with it.
    const body: ApproveBody = {
      ...(customerId ? { customerId } : {}),
      ...(renews ? {} : { licensee: licensee.trim() }),
      expiresOn,
    };
    try {
      const res = await m.mutateAsync({ params: { id: r.id }, body });
      notify.success(`Approved: ${res.license.id} for ${res.license.licensee}`);
      onClose();
    } catch (err) {
      const field = firstField(err, APPROVE_FIELDS);
      if (field) navRef.current?.goTo(field === 'customerId' ? 'customer' : 'key');
    }
  };

  const steps: WizardStep[] = [
    {
      id: 'customer',
      label: 'Customer',
      description: 'Personal keys go to a person customer; the requester becomes its member.',
      content: (
        <VStack gap={4}>
          <ChoiceCards
            label="Customer"
            isLabelHidden
            value={mode}
            onChange={(v) => {
              setMode(v as 'default' | 'other');
              fe.edited('customerId');
            }}
            choices={[
              known
                ? { value: 'default', label: known.name, description: `The person customer the portal already has for ${r.requesterEmail}.` }
                : { value: 'default', label: 'New person customer', description: `Created as ${r.name}, with ${r.requesterEmail} as its member.` },
              {
                value: 'other',
                label: known ? 'Another existing person customer' : 'Existing person customer',
                description: 'For someone the portal already knows under another address. Organizations are not offered.',
              },
            ]}
            error={mode === 'default' ? fe.field('customerId') : undefined}
          />
          {mode === 'other' ? (
            <CustomerField
              kind="person"
              value={customer}
              onChange={(c) => {
                setCustomer(c);
                fe.edited('customerId');
              }}
              isRequired
              error={(showErrors.customer ? customerError() : null) ?? fe.field('customerId')}
            />
          ) : null}
        </VStack>
      ),
      validate: () => {
        setShowErrors((s) => ({ ...s, customer: true }));
        return customerError() ? 'Choose a person customer, or keep the default' : null;
      },
    },
    {
      id: 'key',
      label: 'Key',
      description: renews ? `A renewal of ${renews.licenseId}: ${PERSONAL_MAX_NODES} nodes, every feature.` : `A personal key: ${PERSONAL_MAX_NODES} nodes, every feature.`,
      content: (
        <VStack gap={4}>
          {renews ? (
            <Text as="p">
              Approving renews {renews.licenseId}, whose current key expires {utcDay(renews.expiresOn)}. The renewed key keeps the license's licensee.
            </Text>
          ) : (
            <TextField
              label="Licensee"
              value={licensee}
              onChange={(v) => {
                setLicensee(v);
                fe.edited('licensee');
              }}
              isRequired
              help="Printed on the key. The requester asked for the name below."
              error={(showErrors.key ? nameError(licensee, 'the licensee') : null) ?? fe.field('licensee')}
            />
          )}
          <DateField
            label="Expires on (UTC)"
            value={expiresOn}
            onChange={(v) => {
              setExpiresOn(v);
              fe.edited('expiresOn');
            }}
            min={bounds.min}
            max={bounds.max}
            presets={termPresets('personal', renews ? renewalBase(today, renews.expiresOn) : today, bounds.max)}
            isRequired
            help={!dateError(expiresOn, bounds) ? `Keeps working on nodes until ${utcDay(graceEndsOn(expiresOn))}.` : undefined}
            error={(showErrors.key ? dateError(expiresOn, bounds) : null) ?? fe.field('expiresOn')}
          />
        </VStack>
      ),
      validate: () => {
        setShowErrors((s) => ({ ...s, key: true }));
        return keyError();
      },
    },
    {
      id: 'review',
      label: 'Review',
      description: 'Approving signs the key and mails it to the requester.',
      content: (
        <VStack gap={3}>
          <WizardNavHandle navRef={navRef} />
          <MetadataList>
            <ReviewRow label="Customer" step="customer" what="customer">
              {mode === 'other' ? (customer?.name ?? '–') : known ? `${known.name} (the person customer of ${r.requesterEmail})` : `${r.name} (new person, member ${r.requesterEmail})`}
            </ReviewRow>
            {renews ? (
              <MetadataListItem label="Renews">
                {renews.licenseId}, current key expires {utcDay(renews.expiresOn)}
              </MetadataListItem>
            ) : (
              <ReviewRow label="Licensee" step="key" what="licensee">
                {licensee.trim()}
              </ReviewRow>
            )}
            <MetadataListItem label="Plan">{planLabel('personal')}</MetadataListItem>
            <MetadataListItem label="Nodes">{PERSONAL_MAX_NODES}</MetadataListItem>
            <MetadataListItem label="Features">{ALL_FEATURES_LABEL}</MetadataListItem>
            <ReviewRow label="Expires (UTC)" step="key" what="expiry">
              {utcDay(expiresOn)}
            </ReviewRow>
            <ReviewRow label="Keeps working until (UTC)" step="key" what="expiry">
              {dateError(expiresOn, bounds) ? '–' : utcDay(graceEndsOn(expiresOn))}
            </ReviewRow>
          </MetadataList>
          <MetadataList title="The request">
            <MetadataListItem label="Use">{r.use}</MetadataListItem>
            <MetadataListItem label="Certified">
              {utcDateTime(r.attestation.at)}: {r.attestation.statement ?? 'by an admin'}
            </MetadataListItem>
          </MetadataList>
          <Text type="supporting">
            {renews ? 'Approving signs the renewed key and mails it to the requester.' : 'If the person meanwhile holds a personal license, the server renews that one instead.'}
          </Text>
        </VStack>
      ),
      // The Stepper allows changing an earlier step and coming back: check them again.
      validate: () => {
        const problem = customerError() ? { step: 'customer', message: 'Choose a person customer, or keep the default' } : keyError() ? { step: 'key', message: keyError() ?? '' } : null;
        if (!problem) return null;
        setShowErrors({ customer: true, key: true });
        navRef.current?.goTo(problem.step);
        return problem.message;
      },
    },
  ];

  const error = fe.summary ? <Banner status="error" title="The request was not approved" description={fe.summary} /> : null;
  return <Wizard label="Approve request" steps={steps} finishLabel="Approve and send key" onFinish={approve} isFinishing={m.isPending} error={error} />;
}
