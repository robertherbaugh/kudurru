// /system (system.read): how the portal is configured, read-only and
// without secrets (GET /api/v1/system): the signing key (kid, loaded,
// trusted, trusted since, the keys signed per kid), sign-in (issuer,
// client, scopes, groups claim, role mappings, admin claim rules, default
// role, member auto-link), mail, the public form, the policy constants and
// the build. Operators change these by configuration and release.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Grid } from '@astryxdesign/core/Grid';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
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
                <MetadataListItem label="Admin notifications">{s.mail.adminNotify ?? 'None'}</MetadataListItem>
              </MetadataList>
            </Panel>
            <Panel title="Public form">
              <MetadataList>
                <MetadataListItem label="Enabled">{yesNo(s.publicForm.enabled)}</MetadataListItem>
                <MetadataListItem label="Origins">{list(s.publicForm.origins)}</MetadataListItem>
                <MetadataListItem label="Per address and hour">{s.publicForm.limitPerIpPerHour}</MetadataListItem>
                <MetadataListItem label="Per email and day">{s.publicForm.limitPerEmailPerDay}</MetadataListItem>
                <MetadataListItem label="Automatic approval">
                  <HStack gap={1} vAlign="center">
                    {yesNo(s.publicForm.personalAutoApprove)}
                    <Text type="supporting">up to {s.publicForm.personalAutoApproveMaxPerDay} a day</Text>
                  </HStack>
                </MetadataListItem>
                <MetadataListItem label="Admin signatures">up to {s.issuing.maxPerActorPerDay} per admin a day</MetadataListItem>
              </MetadataList>
            </Panel>
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
