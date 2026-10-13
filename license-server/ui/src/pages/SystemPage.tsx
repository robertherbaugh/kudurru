// /system (system.read): how the portal is configured, read-only and
// without secrets (GET /api/v1/system): the signing key (kid, loaded,
// trusted, trusted since, the keys signed per kid), sign-in (issuer,
// client, scopes, groups claim, role mappings, admin claim rules, default
// role, member auto-link), mail, the public form, the approval of personal
// keys (required or automatic, who is mailed about new requests), the
// policy constants and the build. Operators change these by configuration
// and release.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Grid } from '@astryxdesign/core/Grid';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { SystemInfo } from '@shared/api';
import { ROLES, roleLabel } from '@shared/permissions';
import { useSystem } from '../api/hooks';
import { utcDateTime } from '../lib/dates';
import { DEV_BANNER } from '../lib/words';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { formatExact } from '../ui/lib/format';

function yesNo(v: boolean) {
  return v ? <Badge variant="success" label="Yes" /> : <Badge variant="neutral" label="No" />;
}

function list(items: readonly string[], empty = 'None'): string {
  return items.length ? items.join(', ') : empty;
}

function duration(seconds: number): string {
  if (seconds % 3600 === 0) return `${seconds / 3600} h`;
  if (seconds % 60 === 0) return `${seconds / 60} min`;
  return `${seconds} s`;
}

export default function SystemPage() {
  const q = useSystem();
  const s = q.data;
  return (
    <Page>
      <PageHeader title="System" description="How this portal is configured. Read-only: operators change it through the environment and releases." />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {s ? (
        <>
          {s.devMode ? <Banner status="warning" title={DEV_BANNER} /> : null}
          {!s.signing.available ? <Banner status="error" title="Signing is unavailable" description={s.signing.unavailableReason ?? 'No signing key is loaded.'} /> : null}
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <Panel title="Signing">
              <MetadataList>
                <MetadataListItem label="Signing key (kid)">{s.signing.kid ? <Text type="code">{s.signing.kid}</Text> : 'None'}</MetadataListItem>
                <MetadataListItem label="Loaded">{yesNo(s.signing.available)}</MetadataListItem>
                <MetadataListItem label="Trusted by kudurru">{yesNo(s.signing.trusted)}</MetadataListItem>
                <MetadataListItem label="Trusted since">{s.signing.trustedSince ? `kudurru ${s.signing.trustedSince}` : 'Not recorded'}</MetadataListItem>
                <MetadataListItem label="Test key">{s.signing.testKey ? <Badge variant="warning" label="Yes: no release trusts it" /> : yesNo(false)}</MetadataListItem>
                <MetadataListItem label="Trusted kids">{list(s.signing.trustedKids)}</MetadataListItem>
                <MetadataListItem label="Keys signed">
                  {s.signing.keysByKid.length ? s.signing.keysByKid.map((k) => `${k.kid}: ${formatExact(k.count)}`).join(', ') : 'None yet'}
                </MetadataListItem>
              </MetadataList>
            </Panel>
            <Panel title="Sign-in">
              <MetadataList>
                <MetadataListItem label="Issuer">{s.signIn.issuer}</MetadataListItem>
                <MetadataListItem label="Client ID">
                  <Text type="code">{s.signIn.clientId}</Text>
                </MetadataListItem>
                <MetadataListItem label="Button label">{s.signIn.displayName}</MetadataListItem>
                <MetadataListItem label="Scopes">{list(s.signIn.scopes)}</MetadataListItem>
                <MetadataListItem label="Groups claim">{s.signIn.groupsClaim}</MetadataListItem>
                <MetadataListItem label="ID token algorithm">{s.signIn.idTokenAlg ?? 'From discovery'}</MetadataListItem>
                {ROLES.map((r) => (
                  <MetadataListItem key={r} label={`${roleLabel(r)} groups`}>
                    {list(s.signIn.roleMappings[r] ?? [])}
                  </MetadataListItem>
                ))}
                <MetadataListItem label="Admin claim rules">{s.signIn.adminClaimRules.length ? s.signIn.adminClaimRules.map((c) => `${c.claim}=${c.value}`).join(', ') : 'None'}</MetadataListItem>
                <MetadataListItem label="Default role">{s.signIn.defaultRole === 'none' ? 'None (unmapped sign-ins are refused)' : roleLabel(s.signIn.defaultRole)}</MetadataListItem>
                <MetadataListItem label="Member auto-link">{yesNo(s.signIn.memberAutoLink)}</MetadataListItem>
                <MetadataListItem label="Sessions">
                  idle {duration(s.sessions.idleTimeoutSeconds)}, at most {duration(s.sessions.lifetimeSeconds)}
                </MetadataListItem>
              </MetadataList>
            </Panel>
            <Panel title="Mail">
              <MetadataList>
                <MetadataListItem label="Configured">{yesNo(s.mail.configured)}</MetadataListItem>
                <MetadataListItem label="From">{s.mail.from}</MetadataListItem>
                <MetadataListItem label="Reply to">{s.mail.replyTo}</MetadataListItem>
              </MetadataList>
            </Panel>
            <Panel title="Public form">
              <MetadataList>
                <MetadataListItem label="Enabled">{yesNo(s.publicForm.enabled)}</MetadataListItem>
                <MetadataListItem label="Origins">{list(s.publicForm.origins)}</MetadataListItem>
                <MetadataListItem label="Per address and hour">{s.publicForm.limitPerIpPerHour}</MetadataListItem>
                <MetadataListItem label="Per email and day">{s.publicForm.limitPerEmailPerDay}</MetadataListItem>
                <MetadataListItem label="Admin signatures">up to {s.issuing.maxPerActorPerDay} per admin a day</MetadataListItem>
              </MetadataList>
            </Panel>
            <ApprovalPanel s={s} />
            <Panel title="Policy">
              <MetadataList>
                <MetadataListItem label="Personal key">
                  {s.policy.personalMaxNodes} nodes, {s.policy.personalTermDays} days, renewable {s.policy.renewWindowDays} days ahead
                </MetadataListItem>
                <MetadataListItem label="Trial">at most {s.policy.trialMaxDays} days</MetadataListItem>
                <MetadataListItem label="Business term proposed">{s.policy.businessDefaultTermDays} days</MetadataListItem>
                <MetadataListItem label="Longest term">{formatExact(s.policy.maxTermDays)} days</MetadataListItem>
                <MetadataListItem label="Most nodes">{formatExact(s.policy.maxNodes)}</MetadataListItem>
                <MetadataListItem label="Grace on nodes">{s.policy.graceDays} days</MetadataListItem>
                <MetadataListItem label="Expiry warning">{s.policy.expiryWarningDays} days</MetadataListItem>
                <MetadataListItem label="Confirmation link">{s.policy.confirmTtlHours} hours</MetadataListItem>
                <MetadataListItem label="Invite link">{s.policy.inviteTtlDays} days</MetadataListItem>
                <MetadataListItem label="Approval link">{s.policy.approvalLinkTtlDays} days, once</MetadataListItem>
              </MetadataList>
            </Panel>
            <Panel title="Build">
              <MetadataList>
                <MetadataListItem label="Version">{s.build.version}</MetadataListItem>
                <MetadataListItem label="Commit">
                  <Text type="code">{s.build.commit}</Text>
                </MetadataListItem>
                <MetadataListItem label="Built">{s.build.builtAt ? utcDateTime(s.build.builtAt) : 'Unknown'}</MetadataListItem>
              </MetadataList>
            </Panel>
          </Grid>
        </>
      ) : null}
    </Page>
  );
}

/** Why nobody is mailed about a new request (mail off, or no address to mail), or null when someone is. */
function nobodyMailed(s: SystemInfo): string | null {
  if (!s.mail.configured) return 'Mail is not configured (SMTP_URL), so no approval link goes out: requests wait on the Requests page.';
  if (s.approval.recipients.length === 0) return 'They wait on the Requests page without a mail. Set ADMIN_NOTIFY_EMAIL, or give a license admin a verified email address.';
  return null;
}

/** Who approves personal keys, and who is mailed when a request opens. */
function ApprovalPanel({ s }: { s: SystemInfo }) {
  const a = s.approval;
  const why = nobodyMailed(s);
  return (
    <Panel title="Approval" description="Personal key requests and renewals from the kudurru.io form and the portal.">
      {why ? <Banner status="warning" title="Nobody is mailed about new requests" description={why} /> : null}
      <MetadataList>
        <MetadataListItem label="Personal keys">
          {a.required ? 'Approval required: a license admin approves each key and renewal' : `Automatic, up to ${s.publicForm.personalAutoApproveMaxPerDay} a day`}
        </MetadataListItem>
        <MetadataListItem label={s.mail.configured ? 'Mailed about new requests' : 'Would be mailed about new requests'}>
          <VStack gap={0.5}>
            {a.fromAdmins ? <Text>License admins and superadmins with a verified email</Text> : null}
            {a.recipients.map((r) => (
              <Text key={r} type={a.fromAdmins ? 'supporting' : 'body'}>
                {r}
              </Text>
            ))}
            {!a.fromAdmins && a.recipients.length === 0 ? <Text>None</Text> : null}
          </VStack>
        </MetadataListItem>
      </MetadataList>
    </Panel>
  );
}
