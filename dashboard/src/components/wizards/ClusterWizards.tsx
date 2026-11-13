// The enrolled SQLite cluster wizards of the Nodes page
// (docs/deploy/sqlite-cluster.md; dev-docs/milestones/m3/ui.md "Pages, wizards and dialogs"):
//
// - Activate: review this dormant standalone node's enrollment, name its
//   member and internal address, activate it as the first primary.
// - Invite: review the primary's enrollment, name the new member, issue a
//   one-time invitation and show it once with the join command and the
//   Compose changes for the new node.
// - Promote: review this follower's promotion eligibility, confirm the node
//   by name and promote it.
//
// Every wizard reads its review fresh when it opens and sends exactly the
// values the operator confirmed: ticking the confirmation snapshots the
// review on screen, reading again or unticking drops the snapshot, and the
// finishing action checks every step again (the stepper can jump past the
// review step), so a re-read review is never sent without a new
// confirmation. While the command is in flight the dialog cannot be closed.
// It sends its command once: after a 409 (the reviewed state
// changed) or an answer that leaves the outcome unknown it sends nothing more
// and says to review again (lib/cluster.ts commandOutcome). The invitation is
// a bearer secret: it is requested with a plain call (no query or mutation
// cache), kept only in the state of the step that shows it, never logged and
// never put in a URL; closing the dialog drops it.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { qk, useApi } from '../../api/context';
import type { ClusterActivationReceipt, ClusterEnrollmentInfo, ClusterJoinTokenReceipt, ClusterPromotionInfo, ClusterPromotionReceipt } from '../../api/types';
import { CLUSTER_DOCS, commandOutcome, composeCommands, composeSnippet, invitationConflictText, joinCommand, lockedText, memberAddressError, memberNameError, promotionReasonText, reasonError, type CommandOutcome } from '../../lib/cluster';
import { formatDateTime, formatRelative } from '../../lib/format';
import { DialogForm } from '../FormDialog';
import { ErrorBanner, errorText, Loading } from '../Feedback';
import { SelectField } from '../fields/SelectField';
import { CheckboxField, TextField } from '../fields/TextField';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../Wizard';

/** A little longer than the server's own 30-second bound of each command. */
const COMMAND_TIMEOUT_MS = 35_000;

/** Query keys of the cluster reads (the page's and each wizard's own fresh review). */
export const clusterKeys = {
  enrollment: ['cluster', 'enrollment'] as const,
  promotion: ['cluster', 'promotion'] as const,
  anchors: ['cluster', 'anchors'] as const,
  enrollmentReview: ['cluster', 'enrollment', 'review'] as const,
  promotionReview: ['cluster', 'promotion', 'review'] as const,
};

/** Rereads what a command may have changed (reads only; nothing is sent again). */
export function refreshCluster(qc: QueryClient): void {
  for (const key of [qk.cluster, clusterKeys.enrollment, clusterKeys.promotion, clusterKeys.anchors]) void qc.invalidateQueries({ queryKey: key, exact: true });
}

/** A review read once per wizard opening: never refetched behind the operator's back. */
function useReview<T>(key: readonly unknown[], read: () => Promise<T>) {
  return useQuery({ queryKey: key, queryFn: read, staleTime: Infinity, gcTime: 0, retry: false, refetchOnWindowFocus: false, refetchInterval: false, refetchOnReconnect: false });
}

/**
 * The review the operator confirmed: a snapshot of the data on screen taken
 * when the box is ticked. It counts only while that same data is shown (not
 * while a read is running); the command sends the snapshot, never live data.
 */
function useConfirmed<T>(q: { data: T | undefined; isFetching: boolean }) {
  const [snapshot, setSnapshot] = useState<T>();
  const reviewed = snapshot !== undefined && !q.isFetching && q.data === snapshot ? snapshot : undefined;
  return { reviewed, confirmed: reviewed !== undefined, setConfirmed: (v: boolean) => setSnapshot(v ? q.data : undefined) };
}

/** Tells the dialog while a command is in flight (it then cannot be closed). */
function usePendingReport(pending: boolean, onPending: ((pending: boolean) => void) | undefined) {
  useEffect(() => {
    onPending?.(pending);
    // onPending is a new function on every render of the dialog.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pending]);
}

/** Sends one command at most once; after a stale or uncertain answer it stays locked. */
function useOnce() {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [locked, setLocked] = useState<Exclude<CommandOutcome, 'refused'>>();
  const inFlight = useRef(false);
  const qc = useQueryClient();
  const run = async <T,>(send: (signal: AbortSignal) => Promise<T>, done: (result: T) => void) => {
    if (inFlight.current || locked) return;
    inFlight.current = true;
    setPending(true);
    setError(undefined);
    try {
      const result = await send(AbortSignal.timeout(COMMAND_TIMEOUT_MS));
      done(result);
    } catch (err) {
      setError(err);
      const outcome = commandOutcome(err);
      if (outcome !== 'refused') {
        setLocked(outcome);
        refreshCluster(qc);
      }
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  };
  return { pending, error, locked, run };
}

function CommandError({ error, locked, what, stale }: { error: unknown; locked: Exclude<CommandOutcome, 'refused'> | undefined; what: string; stale?: { title: string; text: string } }) {
  if (error === undefined) return null;
  if (!locked) return <ErrorBanner error={error} />;
  const title = locked === 'stale' ? (stale?.title ?? 'The reviewed state changed') : 'Outcome unknown';
  const text = locked === 'stale' && stale ? stale.text : lockedText(locked, what);
  return <Banner status="warning" title={title} description={`${text} The server said: ${errorText(error)}`} />;
}

function Mono({ children }: { children: ReactNode }) {
  return (
    <Text type="code" size="sm">
      {children}
    </Text>
  );
}

/** The four reviewed identities of GET /cluster/enrollment. */
function EnrollmentFacts({ review }: { review: ClusterEnrollmentInfo }) {
  return (
    <MetadataList>
      <MetadataListItem label="Mode">{`${review.mode}${review.dormant ? ' (dormant)' : ''}`}</MetadataListItem>
      <MetadataListItem label="Cluster ID">
        <Mono>{review.clusterID}</Mono>
      </MetadataListItem>
      <MetadataListItem label="Member ID">
        <Mono>{review.memberID}</Mono>
      </MetadataListItem>
      <MetadataListItem label="Source digest">
        <Mono>{review.sourceDigest}</Mono>
      </MetadataListItem>
      <MetadataListItem label="Owner digest">
        <Mono>{review.ownerDigest}</Mono>
      </MetadataListItem>
    </MetadataList>
  );
}

interface ReviewStepProps<T> {
  q: { data: T | undefined; error: unknown; isFetching: boolean; refetch: () => unknown };
  confirmed: boolean;
  setConfirmed: (v: boolean) => void;
  confirmLabel: string;
  children: (data: T) => ReactNode;
}

function ReviewStep<T>({ q, confirmed, setConfirmed, confirmLabel, children }: ReviewStepProps<T>) {
  return (
    <VStack gap={3}>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isFetching ? <Loading label="Reading the current state" /> : q.data ? children(q.data) : null}
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Button
          label="Read again"
          variant="secondary"
          size="sm"
          isDisabled={q.isFetching}
          onClick={() => {
            setConfirmed(false);
            void q.refetch();
          }}
        />
        <Text type="supporting">Reading again clears your confirmation.</Text>
      </HStack>
      <CheckboxField label={confirmLabel} value={confirmed} onChange={setConfirmed} isDisabled={!q.data || q.isFetching} />
    </VStack>
  );
}

// ---------------------------------------------------------------------------
// Activate

function ActivateWizard({ node, onActivated, onPending }: { node: string; onActivated: (r: ClusterActivationReceipt) => void; onPending?: (pending: boolean) => void }) {
  const api = useApi();
  const q = useReview(clusterKeys.enrollmentReview, () => api.cluster.enrollment());
  const { reviewed, confirmed, setConfirmed } = useConfirmed(q);
  const [name, setName] = useState(node);
  const [address, setAddress] = useState('');
  const [reason, setReason] = useState('');
  const once = useOnce();
  usePendingReport(once.pending, onPending);
  const review = q.data;
  const steps: WizardStep[] = [
    {
      id: 'review',
      label: 'Review',
      description: "Check this node's enrollment. Activation is bound to exactly these values.",
      content: (
        <ReviewStep q={q} confirmed={confirmed} setConfirmed={setConfirmed} confirmLabel="I reviewed this node's enrollment">
          {(r) => <EnrollmentFacts review={r} />}
        </ReviewStep>
      ),
      validate: () =>
        !review || q.isFetching
          ? 'Wait until the enrollment is read.'
          : review.mode !== 'standalone' || !review.dormant
            ? `Only a dormant standalone node can be activated; this node is ${review.mode}${review.dormant ? '' : ' and not dormant'}.`
            : !confirmed
              ? 'Confirm that you reviewed the enrollment.'
              : null,
    },
    {
      id: 'member',
      label: 'Member',
      description: 'This node becomes the first member and the primary. No command changes the name or the address later.',
      content: (
        <VStack gap={3}>
          <TextField label="Member name" value={name} onChange={setName} mono error={name ? (memberNameError(name) ?? undefined) : undefined} help="Use this node's node.name: up to 63 lowercase letters, digits and hyphens." />
          <TextField label="Internal address" value={address} onChange={setAddress} mono placeholder="10.0.0.11:8082" error={address ? (memberAddressError(address) ?? undefined) : undefined} help="host:port of this node's internal listener (TLS, port 8082 by default) that every other member can reach." />
          <TextField label="Reason" value={reason} onChange={setReason} help="Recorded in the audit log." />
        </VStack>
      ),
      validate: () => memberNameError(name) ?? memberAddressError(address) ?? reasonError(reason),
    },
    {
      id: 'activate',
      label: 'Activate',
      description: 'Check the values, then activate.',
      content: (
        <VStack gap={3}>
          <MetadataList>
            <ReviewRow label="Member name" step="member" what="member name">
              <Mono>{name}</Mono>
            </ReviewRow>
            <ReviewRow label="Internal address" step="member" what="internal address">
              <Mono>{address}</Mono>
            </ReviewRow>
            <ReviewRow label="Reason" step="member" what="reason">
              {reason}
            </ReviewRow>
            <ReviewRow label="Cluster ID" step="review" what="review">
              <Mono>{review?.clusterID}</Mono>
            </ReviewRow>
          </MetadataList>
          <Banner
            status="info"
            title="One request, then a restart"
            description="The dashboard sends the activation once and never repeats it. After it commits, restart kudurru on this node to start the member listener. Activate at a quiet moment: a query that arrives while the node switches can be answered SERVFAIL."
          />
        </VStack>
      ),
    },
  ];
  return (
    <Wizard
      label="Activate cluster"
      steps={steps}
      finishLabel="Activate cluster"
      isFinishing={once.pending}
      isFinishDisabled={!!once.locked}
      error={<CommandError error={once.error} locked={once.locked} what="activation" />}
      revalidateOnFinish
      isCancelDisabled={once.pending}
      onFinish={() =>
        reviewed
          ? once.run(
              (signal) => api.cluster.activate({ clusterID: reviewed.clusterID, memberID: reviewed.memberID, sourceDigest: reviewed.sourceDigest, ownerDigest: reviewed.ownerDigest, name, address, reason }, signal),
              onActivated,
            )
          : undefined
      }
    />
  );
}

export function ActivateDialog({ isOpen, onClose, node, onActivated }: { isOpen: boolean; onClose: () => void; node: string; onActivated: (r: ClusterActivationReceipt) => void }) {
  const [busy, setBusy] = useState(false);
  return (
    <WizardDialog isOpen={isOpen} onClose={onClose} title="Activate cluster" what="activation" isBusy={busy}>
      <ActivateWizard node={node} onActivated={onActivated} onPending={setBusy} />
    </WizardDialog>
  );
}

// ---------------------------------------------------------------------------
// Invite

// The primary accepts at most ten minutes on its own clock, and the dashboard
// computes the absolute expiry on the browser's clock: the longest choice
// keeps a minute's margin so that a browser clock a little ahead of the
// primary is not refused.
const EXPIRY_OPTIONS = [
  { value: '2', label: '2 minutes', help: 'Enough when you run the join right away.' },
  { value: '5', label: '5 minutes' },
  { value: '9', label: '9 minutes', help: "The longest the dashboard offers: the primary accepts at most ten minutes on its own clock, and the minute left over covers this computer's clock running a little ahead." },
];

/** The shown-once invitation, the join command and the Compose changes for the new member. */
function InvitationView({ receipt, name, address, version, onDone }: { receipt: ClusterJoinTokenReceipt; name: string; address: string; version: string | undefined; onDone: () => void }) {
  const file = `${name}.invitation`;
  return (
    <DialogForm footer={<Button label="Done" variant="primary" onClick={onDone} />}>
      <Banner
        status="success"
        title={`Invitation for ${name} issued`}
        description={`This is the only time it is shown: the primary keeps only its hash, and the dashboard drops it when you close this dialog. It is valid until ${formatDateTime(receipt.expiresAt)} (${formatRelative(receipt.expiresAt)}).`}
      />
      <Text as="p">
        The invitation is a bearer secret. Copy it into a private file on {name}, for example <Mono>{file}</Mono> (with <Mono>umask 077</Mono>), over a private channel. Keep it out of chat, tickets, logs and shell history.
      </Text>
      <CodeBlock code={receipt.invitation} title="Invitation" isWrapped width="100%" data-testid="cluster-invitation" />
      <Text as="p">
        On {name}, with kudurru stopped, the primary&apos;s encryption key in <Mono>KUDURRU_ENCRYPTION_KEY_FILE</Mono> and a data directory that does not exist yet:
      </Text>
      <CodeBlock code={joinCommand(name, address, file)} title="Binary or package" language="bash" width="100%" />
      <CodeBlock code={composeSnippet(name, address, version)} title="Docker Compose: docker-compose.yml" language="yaml" width="100%" />
      <CodeBlock code={composeCommands(name, address, file)} title="Docker Compose: join, then start" language="bash" width="100%" />
      <Text type="supporting" as="p">
        Require <Mono>&quot;committed&quot;: true</Mono> under <Mono>primary</Mono> and an <Mono>installed</Mono> generation in the join&apos;s result, then start kudurru with the same name, internal address and encryption key, and delete the file. If the join fails, keep the directory and the file and follow{' '}
        <Link href={CLUSTER_DOCS.joinRecover} isExternalLink>
          Recover an incomplete join
        </Link>
        ; never issue a second invitation for the same member as a retry.
      </Text>
    </DialogForm>
  );
}

function InviteWizard({ onIssued, onPending }: { onIssued: (receipt: ClusterJoinTokenReceipt, name: string, address: string) => void; onPending: (pending: boolean) => void }) {
  const api = useApi();
  const q = useReview(clusterKeys.enrollmentReview, () => api.cluster.enrollment());
  const { reviewed, confirmed, setConfirmed } = useConfirmed(q);
  const [name, setName] = useState('');
  const [address, setAddress] = useState('');
  const [minutes, setMinutes] = useState('5');
  const [reason, setReason] = useState('');
  const once = useOnce();
  usePendingReport(once.pending, onPending);
  const review = q.data;
  const steps: WizardStep[] = [
    {
      id: 'review',
      label: 'Review',
      description: "Check the primary's enrollment. The invitation is bound to exactly these values; they change with every change of the primary's data.",
      content: (
        <ReviewStep q={q} confirmed={confirmed} setConfirmed={setConfirmed} confirmLabel="I reviewed the primary's enrollment">
          {(r) => <EnrollmentFacts review={r} />}
        </ReviewStep>
      ),
      validate: () => (!review || q.isFetching ? 'Wait until the enrollment is read.' : review.mode !== 'primary' ? `Only the primary issues invitations; this node is ${review.mode}.` : !confirmed ? 'Confirm that you reviewed the enrollment.' : null),
    },
    {
      id: 'member',
      label: 'New member',
      description: 'The invitation is valid for this one name and internal address. Names and addresses of earlier members stay reserved, also of members you stopped for good.',
      content: (
        <VStack gap={3}>
          <TextField label="Member name" value={name} onChange={setName} mono placeholder="node-b" error={name ? (memberNameError(name) ?? undefined) : undefined} help="The new node's node.name: up to 63 lowercase letters, digits and hyphens." />
          <TextField label="Internal address" value={address} onChange={setAddress} mono placeholder="10.0.0.12:8082" error={address ? (memberAddressError(address) ?? undefined) : undefined} help="host:port of the new node's internal listener that every member can reach." />
          <SelectField label="Valid for" value={minutes} options={EXPIRY_OPTIONS} onChange={setMinutes} help="Counted from the moment you issue it, on this computer's clock; the primary checks it on its own. Run the join within this time." />
          <TextField label="Reason" value={reason} onChange={setReason} help="Recorded in the audit log." />
        </VStack>
      ),
      validate: () => memberNameError(name) ?? memberAddressError(address) ?? reasonError(reason),
    },
    {
      id: 'issue',
      label: 'Issue',
      description: 'Check the values, then issue the invitation.',
      content: (
        <VStack gap={3}>
          <MetadataList>
            <ReviewRow label="Member name" step="member" what="member name">
              <Mono>{name}</Mono>
            </ReviewRow>
            <ReviewRow label="Internal address" step="member" what="internal address">
              <Mono>{address}</Mono>
            </ReviewRow>
            <ReviewRow label="Valid for" step="member" what="validity">
              {`${minutes} minutes`}
            </ReviewRow>
            <ReviewRow label="Reason" step="member" what="reason">
              {reason}
            </ReviewRow>
          </MetadataList>
          <Banner
            status="info"
            title="Shown once"
            description="The next screen shows the invitation once, with the commands for the new node. Have a private channel to that node ready. The dashboard sends the request once and never repeats it."
          />
        </VStack>
      ),
    },
  ];
  return (
    <Wizard
      label="Invite a member"
      steps={steps}
      finishLabel="Issue invitation"
      isFinishing={once.pending}
      isFinishDisabled={!!once.locked}
      error={<CommandError error={once.error} locked={once.locked} what="invitation" stale={{ title: 'The primary refused the invitation', text: invitationConflictText() }} />}
      revalidateOnFinish
      isCancelDisabled={once.pending}
      onFinish={() =>
        reviewed
          ? once.run(
              (signal) =>
                api.cluster.issueJoinToken(
                  {
                    clusterID: reviewed.clusterID,
                    memberID: reviewed.memberID,
                    sourceDigest: reviewed.sourceDigest,
                    ownerDigest: reviewed.ownerDigest,
                    name,
                    address,
                    // The original absolute expiry; the server never renews it.
                    expiresAt: new Date(Date.now() + Number(minutes) * 60_000).toISOString(),
                    reason,
                  },
                  signal,
                ),
              (receipt) => onIssued(receipt, name, address),
            )
          : undefined
      }
    />
  );
}

/** The invitation flow: the wizard, then (only in this component's state) the issued invitation. */
function InviteFlow({ version, onShown, onDone, onPending }: { version: string | undefined; onShown: () => void; onDone: () => void; onPending: (pending: boolean) => void }) {
  const qc = useQueryClient();
  const [issued, setIssued] = useState<{ receipt: ClusterJoinTokenReceipt; name: string; address: string }>();
  if (issued) return <InvitationView receipt={issued.receipt} name={issued.name} address={issued.address} version={version} onDone={onDone} />;
  return (
    <InviteWizard
      onPending={onPending}
      onIssued={(receipt, name, address) => {
        setIssued({ receipt, name, address });
        onShown();
        refreshCluster(qc);
      }}
    />
  );
}

export function InviteDialog({ isOpen, onClose, version }: { isOpen: boolean; onClose: () => void; version: string | undefined }) {
  const [shown, setShown] = useState(false);
  // While the request is in flight the dialog stays open: closing it would
  // drop an invitation the primary may already have committed.
  const [busy, setBusy] = useState(false);
  const close = () => {
    setShown(false);
    onClose();
  };
  return (
    <WizardDialog
      isOpen={isOpen}
      onClose={close}
      title="Invite a member"
      what="invitation"
      isBusy={busy}
      discard={shown ? { title: 'Close the invitation?', description: 'It is not shown again. Copy it first if the new member does not have it yet.', actionLabel: 'Close', cancelLabel: 'Keep it open' } : undefined}
    >
      <InviteFlow version={version} onShown={() => setShown(true)} onDone={close} onPending={setBusy} />
    </WizardDialog>
  );
}

// ---------------------------------------------------------------------------
// Promote

function PromotionFacts({ review }: { review: ClusterPromotionInfo }) {
  return (
    <VStack gap={3}>
      <Banner status={review.eligible ? 'info' : 'warning'} title={review.eligible ? 'Eligible for promotion' : 'Not eligible for promotion'} description={promotionReasonText(review.reason)} />
      <MetadataList>
        <MetadataListItem label="Node">{review.node}</MetadataListItem>
        <MetadataListItem label="Mode">{review.mode}</MetadataListItem>
        <MetadataListItem label="Missed pulls">{review.missedPulls}</MetadataListItem>
        <MetadataListItem label="Primary unreachable for">{`${Math.round(review.unreachableSeconds)} s`}</MetadataListItem>
        <MetadataListItem label="Installed term">
          <Mono>{String(review.installed.term.epoch)}</Mono>
        </MetadataListItem>
        <MetadataListItem label="Installed revision">
          <Mono>{String(review.installed.revision)}</Mono>
        </MetadataListItem>
        <MetadataListItem label="Installed digest">
          <Mono>{review.installed.digest}</Mono>
        </MetadataListItem>
      </MetadataList>
    </VStack>
  );
}

function PromoteWizard({ onPromoted, onPending }: { onPromoted: (r: ClusterPromotionReceipt) => void; onPending: (pending: boolean) => void }) {
  const api = useApi();
  const q = useReview(clusterKeys.promotionReview, () => api.cluster.promotion());
  const { reviewed, confirmed, setConfirmed } = useConfirmed(q);
  const [confirmNode, setConfirmNode] = useState('');
  const [reason, setReason] = useState('');
  // One identifier for this one operator decision.
  const [requestID] = useState(() => `dashboard-${crypto.randomUUID()}`);
  const once = useOnce();
  usePendingReport(once.pending, onPending);
  const review = q.data;
  const steps: WizardStep[] = [
    {
      id: 'review',
      label: 'Review',
      description: "Check this follower's eligibility. The promotion is bound to exactly this installed generation.",
      content: (
        <ReviewStep q={q} confirmed={confirmed} setConfirmed={setConfirmed} confirmLabel="I reviewed this follower's state and the primary is lost">
          {(r) => <PromotionFacts review={r} />}
        </ReviewStep>
      ),
      validate: () => (!review || q.isFetching ? 'Wait until the review is read.' : !review.eligible ? promotionReasonText(review.reason) : !confirmed ? 'Confirm that you reviewed the state.' : null),
    },
    {
      id: 'decide',
      label: 'Confirm',
      description: 'Promotion is epoch fencing, not consensus. Promote one follower only: two followers promoted while they cannot reach each other both accept writes and record a conflict when they meet.',
      content: (
        <VStack gap={3}>
          <TextField label="Type the node name" value={confirmNode} onChange={setConfirmNode} mono help={review ? `Type ${review.node} to confirm that this is the node to promote.` : undefined} />
          <TextField label="Reason" value={reason} onChange={setReason} help="Recorded in the audit log with the promotion." />
        </VStack>
      ),
      validate: () => (!review || confirmNode !== review.node ? `Type ${review?.node ?? 'the node name'} exactly.` : reasonError(reason)),
    },
    {
      id: 'promote',
      label: 'Promote',
      description: 'Check the values, then promote.',
      content: (
        <VStack gap={3}>
          <MetadataList>
            <ReviewRow label="Node" step="decide" what="node confirmation">
              <Mono>{confirmNode}</Mono>
            </ReviewRow>
            <ReviewRow label="Reason" step="decide" what="reason">
              {reason}
            </ReviewRow>
            <MetadataListItem label="Request ID">
              <Mono>{requestID}</Mono>
            </MetadataListItem>
          </MetadataList>
          <Banner
            status="info"
            title="One request"
            description="The server checks again that the primary is unreachable and that this node serves the reviewed data. The dashboard sends the promotion once and never repeats it; if the answer is lost, review the state again before you decide anything else."
          />
        </VStack>
      ),
    },
  ];
  return (
    <Wizard
      label="Promote this follower"
      steps={steps}
      finishLabel={review ? `Promote ${review.node}` : 'Promote'}
      isFinishing={once.pending}
      isFinishDisabled={!!once.locked}
      error={<CommandError error={once.error} locked={once.locked} what="promotion" />}
      revalidateOnFinish
      isCancelDisabled={once.pending}
      onFinish={() =>
        reviewed
          ? once.run((signal) => api.cluster.promote({ memberID: reviewed.memberID, installed: reviewed.installed, confirmNode, requestID, reason }, signal), onPromoted)
          : undefined
      }
    />
  );
}

export function PromoteDialog({ isOpen, onClose, onPromoted }: { isOpen: boolean; onClose: () => void; onPromoted: (r: ClusterPromotionReceipt) => void }) {
  const [busy, setBusy] = useState(false);
  return (
    <WizardDialog isOpen={isOpen} onClose={onClose} title="Promote this follower" what="promotion" isBusy={busy}>
      <PromoteWizard onPromoted={onPromoted} onPending={setBusy} />
    </WizardDialog>
  );
}
