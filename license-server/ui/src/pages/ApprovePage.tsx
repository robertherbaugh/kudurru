// /approve?token= (anonymous, outside the session gate): the one-click
// approval link of the mail a license admin gets when a personal key
// request opens (a token per recipient, APPROVAL_LINK_TTL_DAYS days, used
// once). Opening the page changes nothing, because mail scanners open
// links: it previews the request and the terms approving signs (POST
// /api/v1/public/approvals/preview), and only a click decides (POST
// /api/v1/public/approvals/decide). "Approve and send the key" signs the
// key and mails it to the requester; "Reject" needs a note, which is
// mailed to them. A used or expired link, or a request decided meanwhile
// (also in the portal), still previews and says so, pointing at the
// portal's Requests page (the terms only while the request is open). A
// refused click loads the preview again and lets it say what changed: a
// 409 on a request still open (its default terms cannot be signed now)
// keeps Reject. The key itself never shows here.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Center } from '@astryxdesign/core/Center';
import { Heading } from '@astryxdesign/core/Heading';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMutation } from '@tanstack/react-query';
import { useState } from 'react';
import type { ApprovalPreview, ApprovalResult } from '@shared/api';
import { ALL_FEATURES_LABEL, APPROVAL_LINK_TTL_DAYS, planLabel } from '@shared/catalog';
import { graceEndsOn } from '@shared/dates';
import { ApiError } from '../api/client';
import { useApi } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useApprovalPreview } from '../api/hooks';
import { utcDateTime, utcDay } from '../lib/dates';
import { PRODUCT_NAME, REQUEST_SOURCE_LABEL, REVIEW_REASON_LABEL } from '../lib/words';
import { BrandLogo } from '../ui/components/BrandLogo';
import { errorText, Loading } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { TextAreaField } from '../ui/fields/TextField';
import { useLocation } from '../ui/lib/router';

/** The portal's list of requests, where a signed-in license admin decides without a link. */
const REQUESTS_HREF = '/requests';

/** The longest rejection note (the server checks it too). */
const NOTE_MAX = 1000;

const portalLink = <Link href={REQUESTS_HREF}>Open the requests in the portal</Link>;

/** Whether an error means this link can no longer decide anything (used or expired meanwhile). */
function linkSpent(err: unknown): boolean {
  return err instanceof ApiError && err.key === 'invalid_token';
}

/**
 * Whether an error is the server's 409: the request was decided meanwhile,
 * or (approve only) its default terms cannot be signed now, e.g. the
 * person already holds a personal key too far ahead to renew. Which of the
 * two shows from the preview loaded again, never from the error alone.
 */
function isConflict(err: unknown): boolean {
  return err instanceof ApiError && err.key === 'conflict';
}

/** Whether the state behind an error is worth loading again: the link or the request may have changed. */
function mayBeStale(err: unknown): boolean {
  return linkSpent(err) || isConflict(err);
}

/** Why the preview failed, in words. */
export function previewErrorWords(err: unknown): { title: string; description: string } {
  if (err instanceof ApiError) {
    if (err.key === 'invalid_token' || err.key === 'invalid') {
      return { title: 'This approval link does not work', description: 'The portal did not mail this link, or its request no longer exists. Decide on the Requests page of the portal instead.' };
    }
    if (err.key === 'rate_limited') return { title: 'Too many attempts', description: `Wait a little and try again${err.retryAfter ? ` in ${err.retryAfter} seconds` : ''}.` };
  }
  return { title: 'The request could not be loaded', description: errorText(err) };
}

/** Why a decision failed, in words. */
export function decideErrorWords(err: unknown, decision: 'approve' | 'reject'): { title: string; description: string } {
  if (err instanceof ApiError) {
    switch (err.key) {
      case 'invalid_token':
        // The server's sentence says used or expired, and where to go instead (its hint, the portal URL, is the banner's link).
        return { title: 'This approval link no longer works', description: err.message };
      case 'rate_limited':
        return { title: 'Too many attempts', description: `Wait a little and try again${err.retryAfter ? ` in ${err.retryAfter} seconds` : ''}.` };
      case 'issuing_quota':
        return { title: 'The daily signing limit is reached', description: errorText(err) };
      case 'signing_unavailable':
      case 'unavailable':
        return { title: 'Keys cannot be signed right now', description: errorText(err) };
      default:
        break;
    }
  }
  return { title: decision === 'approve' ? 'The request was not approved' : 'The request was not rejected', description: errorText(err) };
}

/** The headline of a request that can no longer be decided here. */
function decidedTitle(p: ApprovalPreview): string {
  const by = p.decidedBy ? ` by ${p.decidedBy}` : '';
  switch (p.status) {
    case 'approved':
      return `This request was already approved${by}`;
    case 'rejected':
      return `This request was already rejected${by}`;
    case 'canceled':
      return 'This request was canceled';
    case 'open':
      return 'This request is still open';
  }
}

export default function ApprovePage() {
  const client = useApi();
  const loc = useLocation();
  const token = loc.search.get('token') ?? '';
  const preview = useApprovalPreview(token);
  const approve = useMutation({
    mutationFn: () => client.call('approvalDecide', { body: { token, decision: 'approve' } }),
    onError: (err) => {
      // Show what happened instead (decided in the portal meanwhile, or the link was used); a request still open stays decidable.
      if (mayBeStale(err)) void preview.refetch();
    },
  });
  const [rejecting, setRejecting] = useState(false);
  const [rejected, setRejected] = useState<ApprovalResult | null>(null);
  const result = approve.data ?? rejected;

  let body;
  if (!token) {
    body = (
      <Banner
        status="warning"
        title="This page needs the link from your mail"
        description="Open the approval link in the mail about the request, or decide on the Requests page of the portal."
        endContent={portalLink}
      />
    );
  } else if (!preview.data) {
    // A failed refetch after a refused decision keeps the request it had (data stays); only a first load that failed ends here.
    const w = preview.error ? previewErrorWords(preview.error) : null;
    body = w ? <Banner status="error" title={w.title} description={w.description} endContent={portalLink} /> : <Loading label="Loading the request" />;
  } else {
    const p = preview.data;
    const decidable = p.status === 'open' && !p.expired && !result;
    const failed = approve.error && decidable ? decideErrorWords(approve.error, 'approve') : null;
    body = (
      <VStack gap={4}>
        {result ? <Outcome result={result} preview={p} /> : null}
        {!result && p.status !== 'open' ? <Banner status="info" title={decidedTitle(p)} description="Nothing is left to decide here." endContent={portalLink} /> : null}
        {!result && p.status === 'open' && p.expired ? (
          <Banner
            status="warning"
            title="This approval link no longer works"
            description={`It was already used, or it is older than ${APPROVAL_LINK_TTL_DAYS} days. The request is still open: decide on the Requests page of the portal.`}
            endContent={portalLink}
          />
        ) : null}
        <RequestFacts preview={p} />
        {p.status === 'open' && !result ? <Terms preview={p} /> : null}
        {decidable ? (
          <VStack gap={3}>
            <Text as="p" type="supporting">
              This link was mailed to {p.sentTo} and works once. {p.renews ? `Approving renews ${p.renews.licenseId} and mails the new key` : 'Approving signs the key and mails it'} to{' '}
              {p.request.email}; rejecting mails them your note instead.
            </Text>
            {failed ? <Banner status="error" title={failed.title} description={failed.description} endContent={mayBeStale(approve.error) ? portalLink : undefined} /> : null}
            <HStack gap={2} wrap="wrap">
              {/* After a 409 on a request still open, approving with the default terms cannot work (the portal can approve with other terms); rejecting still can. */}
              <Button label="Approve and send the key" variant="primary" isLoading={approve.isPending} isDisabled={mayBeStale(approve.error)} onClick={() => approve.mutate()} />
              <Button label="Reject" variant="secondary" isDisabled={approve.isPending || linkSpent(approve.error)} onClick={() => setRejecting(true)} />
            </HStack>
          </VStack>
        ) : null}
        {/* Closes by itself once the request can no longer be decided here; the banner above says why. */}
        <FormDialog isOpen={rejecting && decidable} onClose={() => setRejecting(false)} title="Reject request" subtitle={`${p.request.name} · ${p.request.email}`} width={520}>
          <RejectForm
            token={token}
            preview={p}
            onClose={() => setRejecting(false)}
            onFinal={() => void preview.refetch()}
            onDone={(r) => {
              setRejected(r);
              setRejecting(false);
            }}
          />
        </FormDialog>
      </VStack>
    );
  }

  return (
    <Center role="main" minHeight="100dvh" padding={4}>
      <Card width="100%" maxWidth={680} padding={6}>
        <VStack gap={4}>
          <VStack gap={2}>
            <BrandLogo tagline alt="kudurru: authority over every zone." />
            <Heading level={1}>Approve a key request</Heading>
            <Text as="p" type="supporting">
              {PRODUCT_NAME}. Free personal keys wait for a license admin's approval.
            </Text>
          </VStack>
          {body}
        </VStack>
      </Card>
    </Center>
  );
}

/** Labels above their values: the mail is often opened on a phone, and the use and the certification are long. */
const LABEL = { position: 'top' } as const;

/** Who asks, and for what. */
function RequestFacts({ preview: p }: { preview: ApprovalPreview }) {
  const r = p.request;
  return (
    <MetadataList title="The request" label={LABEL}>
      <MetadataListItem label="Name on the key">{r.name}</MetadataListItem>
      <MetadataListItem label="Email">{r.email}</MetadataListItem>
      <MetadataListItem label="Use">{r.use}</MetadataListItem>
      <MetadataListItem label="From">{REQUEST_SOURCE_LABEL[r.source]}</MetadataListItem>
      <MetadataListItem label="Requested">{utcDateTime(r.createdAt)}</MetadataListItem>
      <MetadataListItem label="Why it waits">{r.reviewReason ? REVIEW_REASON_LABEL[r.reviewReason] : '–'}</MetadataListItem>
      <MetadataListItem label="Certified">{r.attestation.statement ? `${utcDateTime(r.attestation.at)}: ${r.attestation.statement}` : 'Not recorded'}</MetadataListItem>
    </MetadataList>
  );
}

/** The terms approving signs now (the server computes them from today), for a request still open only. */
function Terms({ preview: p }: { preview: ApprovalPreview }) {
  return (
    <MetadataList title={p.renews ? 'Approving renews' : 'Approving signs'} label={LABEL}>
      {p.renews ? (
        <>
          <MetadataListItem label="License ID">
            <Text type="code">{p.renews.licenseId}</Text>
          </MetadataListItem>
          <MetadataListItem label="Current key expires (UTC)">{utcDay(p.renews.expiresOn)}</MetadataListItem>
        </>
      ) : (
        <MetadataListItem label="License">A new license</MetadataListItem>
      )}
      <MetadataListItem label="Plan">{planLabel(p.terms.plan)}</MetadataListItem>
      <MetadataListItem label="Nodes">{p.terms.nodes}</MetadataListItem>
      <MetadataListItem label="Features">{ALL_FEATURES_LABEL}</MetadataListItem>
      <MetadataListItem label="Expires (UTC)">{utcDay(p.terms.expiresOn)}</MetadataListItem>
      <MetadataListItem label="Keeps working until (UTC)">{utcDay(graceEndsOn(p.terms.expiresOn))}</MetadataListItem>
    </MetadataList>
  );
}

/** The decision this page made. */
function Outcome({ result, preview: p }: { result: ApprovalResult; preview: ApprovalPreview }) {
  if (result.status === 'rejected') {
    return <Banner status="info" title="Rejected" description={`We mailed your note to ${p.request.email}. Nothing was signed.`} />;
  }
  const what = result.licenseId ? `${result.outcome === 'renewed' ? 'Renewed' : 'Issued'} ${result.licenseId}. ` : '';
  return (
    <Banner
      status="success"
      title="Approved: the key is on its way"
      description={`${what}The key goes to ${p.request.email} by email; it is never shown here.`}
      endContent={result.licenseId ? <Link href={`/licenses/${encodeURIComponent(result.licenseId)}`}>Open the license in the portal</Link> : undefined}
    />
  );
}

/** Reject: the note (required, at most NOTE_MAX characters) is mailed to the requester. */
function RejectForm({ token, preview: p, onClose, onFinal, onDone }: { token: string; preview: ApprovalPreview; onClose: () => void; onFinal: () => void; onDone: (r: ApprovalResult) => void }) {
  const client = useApi();
  const [note, setNote] = useState('');
  const [showErrors, setShowErrors] = useState(false);
  const m = useMutation({
    mutationFn: (text: string) => client.call('approvalDecide', { body: { token, decision: 'reject', note: text } }),
    onSuccess: onDone,
    onError: (err) => {
      if (mayBeStale(err)) onFinal();
    },
  });
  const fe = useFieldErrors(m.error, ['note']);
  const err = note.trim() ? (note.length > NOTE_MAX ? `At most ${NOTE_MAX} characters` : null) : 'Write a note for the requester';
  // A refusal that names the note shows under it; everything else in words above the buttons.
  const failed = m.error && !fe.field('note') ? decideErrorWords(m.error, 'reject') : null;
  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (err) return;
        m.mutate(note.trim());
      }}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Reject and send note" variant="destructive" isLoading={m.isPending} isDisabled={linkSpent(m.error)} />
        </>
      }
    >
      <Text as="p">They asked for: {p.request.use}</Text>
      <TextAreaField
        label="Note to the requester"
        value={note}
        onChange={(v) => {
          setNote(v);
          fe.edited('note');
        }}
        isRequired
        help={`Mailed to ${p.request.email}. Say why, and what they can do instead.`}
        error={(showErrors ? err : null) ?? fe.field('note')}
      />
      {failed ? <Banner status="error" title={failed.title} description={failed.description} /> : null}
    </DialogForm>
  );
}
