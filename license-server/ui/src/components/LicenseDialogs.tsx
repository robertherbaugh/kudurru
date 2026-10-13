// The short license actions of an admin's license page, each a FormDialog
// (docs/m3/ui.md: dialogs for short, single-purpose actions):
// - Renew: a later expiry day (presets counted from the later of today and
//   the current expiry), the same claims, optionally mailed;
// - Withdraw: the internal reason, a message for the customer and whether
//   to block the customer's people from the personal flow, with the
//   sentence of how long installed keys keep working; final, so the submit
//   is destructive;
// - Email key: mails the current key (default: the contact email).
// A 400's fields appear under the controls they name (api/fieldErrors).
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { LicenseDetail } from '@shared/api';
import { addDays, graceEndsOn, maxExpiresOn, todayUtc } from '@shared/dates';
import { useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useRouteMutation } from '../api/hooks';
import { utcDay } from '../lib/dates';
import { renewalBase, termBounds, termPresets } from '../lib/terms';
import { emailError } from '../lib/validate';
import { useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { DateField, dateError } from '../ui/fields/DateField';
import { CheckboxField, TextAreaField, TextField } from '../ui/fields/TextField';

interface DialogProps {
  license: LicenseDetail;
  isOpen: boolean;
  onClose: () => void;
}

/** The latest expiry a renewal may set: the issue policy's bound for the plan (lib/terms). */
function renewMax(l: LicenseDetail, today: string): string {
  return termBounds(l.plan, today).max;
}

/** The customer's contact email: undefined while unknown (not loaded, or no customers.read). */
type ContactEmail = string | null | undefined;

export function RenewDialog({ license, isOpen, onClose, contactEmail }: DialogProps & { contactEmail: ContactEmail }) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Renew license" subtitle={`${license.licensee} · ${license.id}`} width={560}>
      <RenewForm license={license} contactEmail={contactEmail} onClose={onClose} />
    </FormDialog>
  );
}

const RENEW_FIELDS = ['expiresOn', 'emailCustomer'];

function RenewForm({ license: l, contactEmail, onClose }: { license: LicenseDetail; contactEmail: ContactEmail; onClose: () => void }) {
  const catalog = useCatalog();
  const notify = useNotify();
  const m = useRouteMutation('licenseRenew');
  const today = catalog.data?.today ?? todayUtc();
  const base = renewalBase(today, l.expiresOn);
  const max = renewMax(l, today);
  const presets = termPresets(l.plan, base, max);
  // Later than the current expiry (spec §6) and tomorrow at the earliest.
  const min = maxExpiresOn(addDays(l.expiresOn, 1), addDays(today, 1));
  const [expiresOn, setExpiresOn] = useState(presets[0]?.value ?? '');
  const [emailCustomer, setEmailCustomer] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const err = dateError(expiresOn, { min, max });
  const mail = catalog.data?.mail.configured ?? false;
  const fe = useFieldErrors(m.error, RENEW_FIELDS);
  // The server refuses the whole renewal when asked to mail a customer without a contact email.
  const canMail = contactEmail !== null;
  const mailIt = emailCustomer && canMail;

  const submit = () => {
    setShowErrors(true);
    if (err) return;
    m.mutate(
      { params: { id: l.id }, body: { expiresOn, ...(mailIt ? { emailCustomer: true } : {}) } },
      {
        onSuccess: (d) => {
          notify.success(`Renewed: the new key is valid through ${utcDay(d.expiresOn)}`);
          onClose();
        },
      },
    );
  };

  return (
    <DialogForm
      onSubmit={submit}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Sign renewed key" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">
        The new key carries the same claims with a later expiry. The current key expires on {utcDay(l.expiresOn)} and keeps working until {utcDay(l.keepsWorkingUntil)}.
      </Text>
      <DateField
        label="New expiry (UTC)"
        value={expiresOn}
        onChange={(v) => {
          setExpiresOn(v);
          fe.edited('expiresOn');
        }}
        min={min}
        max={max}
        presets={presets}
        isRequired
        help={expiresOn && !err ? `Keeps working on nodes until ${utcDay(graceEndsOn(expiresOn))}. Presets count from ${utcDay(base)}.` : `After ${utcDay(l.expiresOn)}, at most ${utcDay(max)}.`}
        error={(showErrors ? err : null) ?? fe.field('expiresOn')}
      />
      {mail ? (
        <CheckboxField
          label="Mail the new key to the customer's contact email"
          value={mailIt}
          onChange={(v) => {
            setEmailCustomer(v);
            fe.edited('emailCustomer');
          }}
          isDisabled={!canMail}
          help={contactEmail ? `Sent to ${contactEmail} after signing.` : canMail ? "Sent to the customer's contact email after signing." : 'The customer has no contact email.'}
          error={fe.field('emailCustomer')}
        />
      ) : null}
      {fe.summary ? <Banner status="error" title="The license was not renewed" description={fe.summary} /> : null}
    </DialogForm>
  );
}

export function WithdrawDialog({ license, isOpen, onClose }: DialogProps) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Withdraw license" subtitle={`${license.licensee} · ${license.id}`} width={560}>
      <WithdrawForm license={license} onClose={onClose} />
    </FormDialog>
  );
}

/** The sentence of what withdrawing cannot do (spec §0.8). */
export function keepsWorkingSentence(keepsWorkingUntil: string): string {
  return `Keys already installed keep working until ${utcDay(keepsWorkingUntil)}.`;
}

const WITHDRAW_FIELDS = ['reason', 'customerMessage', 'blockPersonal'];

function WithdrawForm({ license: l, onClose }: { license: LicenseDetail; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('licenseWithdraw');
  const fe = useFieldErrors(m.error, WITHDRAW_FIELDS);
  const [reason, setReason] = useState('');
  const [message, setMessage] = useState('');
  const [block, setBlock] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const reasonError = !reason.trim() ? 'Say why, for the record' : reason.length > 1000 ? 'At most 1000 characters' : null;

  const submit = () => {
    setShowErrors(true);
    if (reasonError) return;
    m.mutate(
      { params: { id: l.id }, body: { reason: reason.trim(), ...(message.trim() ? { customerMessage: message.trim() } : {}), ...(block ? { blockPersonal: true } : {}) } },
      {
        onSuccess: () => {
          notify.success(`Withdrew ${l.id}`);
          onClose();
        },
      },
    );
  };

  return (
    <DialogForm
      onSubmit={submit}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Withdraw license" variant="destructive" isLoading={m.isPending} />
        </>
      }
    >
      <Banner
        status="warning"
        title={keepsWorkingSentence(l.keepsWorkingUntil)}
        description="Withdrawing records the end of the license, hides its key from the customer, blocks renewal and cancels open requests. It cannot stop a key: nodes check keys offline. It is final; to undo it, issue a new license."
        collapsible={false}
      />
      <TextAreaField
        label="Reason"
        value={reason}
        onChange={(v) => {
          setReason(v);
          fe.edited('reason');
        }}
        isRequired
        help="Internal; the customer never sees it."
        error={(showErrors ? reasonError : null) ?? fe.field('reason')}
      />
      <TextAreaField
        label="Message to the customer"
        value={message}
        onChange={(v) => {
          setMessage(v);
          fe.edited('customerMessage');
        }}
        isOptional
        help="Shown on the customer's license page."
        error={fe.field('customerMessage')}
      />
      <CheckboxField
        label="Refuse new personal keys for the customer's people"
        value={block}
        onChange={(v) => {
          setBlock(v);
          fe.edited('blockPersonal');
        }}
        help="Their member email addresses go on the blocklist: personal key requests from them wait for review."
        error={fe.field('blockPersonal')}
      />
      {fe.summary ? <Banner status="error" title="The license was not withdrawn" description={fe.summary} /> : null}
    </DialogForm>
  );
}

export function EmailKeyDialog({ license, isOpen, onClose, contactEmail }: DialogProps & { contactEmail: ContactEmail }) {
  return (
    <FormDialog isOpen={isOpen} onClose={onClose} title="Email the key" subtitle={`${license.licensee} · ${license.id}`} width={480}>
      <EmailForm license={license} contactEmail={contactEmail} onClose={onClose} />
    </FormDialog>
  );
}

const EMAIL_FIELDS = ['to'];

function EmailForm({ license: l, contactEmail, onClose }: { license: LicenseDetail; contactEmail: ContactEmail; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('licenseEmail');
  const fe = useFieldErrors(m.error, EMAIL_FIELDS);
  const [to, setTo] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  // Unknown (still loading): optional; the server answers 400 `to` when there is no contact email after all.
  const err = emailError(to, { optional: contactEmail !== null });

  const submit = () => {
    setShowErrors(true);
    if (err) return;
    m.mutate(
      { params: { id: l.id }, body: to.trim() ? { to: to.trim() } : {} },
      {
        onSuccess: (d) => {
          notify.success(`Sent the key to ${d.sentTo}`);
          onClose();
        },
      },
    );
  };

  return (
    <DialogForm
      onSubmit={submit}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Send" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <Text as="p">The mail carries the current key, a license.key attachment and how to install it.</Text>
      <TextField
        label="Send to"
        type="email"
        value={to}
        onChange={(v) => {
          setTo(v);
          fe.edited('to');
        }}
        placeholder={contactEmail ?? 'name@example.org'}
        isOptional={contactEmail !== null}
        isRequired={contactEmail === null}
        help={contactEmail ? `Leave empty to use the contact email, ${contactEmail}.` : contactEmail === null ? 'The customer has no contact email.' : "Leave empty to use the customer's contact email."}
        error={(showErrors ? err : null) ?? fe.field('to')}
      />
      {fe.summary ? <Banner status="error" title="The key was not sent" description={fe.summary} /> : null}
    </DialogForm>
  );
}
