// What every page that hands out a key shows (design spec §0.7, §7): the
// key in a wrapped CodeBlock with Download license.key and Copy, the words
// that a key is not a licence (plus the non-commercial notice on personal
// keys), and the panel that says how a node picks a key up. The install
// panel is written fresh: only the file sources are re-read, the flag and
// the environment variable need a restart.
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Copy, Download } from 'lucide-react';
import type { ReactNode } from 'react';
import { KEY_IS_NOT_A_LICENCE, KEY_SOURCES, NONCOMMERCIAL_LICENSE_URL, type Plan } from '@shared/catalog';
import { utcDay } from '../lib/dates';
import { useNotify } from '../ui/components/Feedback';
import { Panel } from '../ui/components/layout/Page';

/** The non-commercial notice every personal key carries. */
export const NONCOMMERCIAL_NOTICE = 'This is a free personal key: it may only be used for personal, non-commercial purposes under the PolyForm Noncommercial License 1.0.0.';

export function KeyNotices({ plan }: { plan: Plan }) {
  return (
    <VStack gap={1}>
      {plan === 'personal' ? (
        <Text as="p" data-testid="noncommercial-notice">
          {NONCOMMERCIAL_NOTICE} <Link href={NONCOMMERCIAL_LICENSE_URL} isExternalLink>Read the license</Link>
        </Text>
      ) : null}
      <Text type="supporting" as="p" data-testid="key-not-licence">
        {KEY_IS_NOT_A_LICENCE}
      </Text>
    </VStack>
  );
}

export interface KeyPanelProps {
  token: string;
  kid: string;
  issuedAt: string;
  expiresOn: string;
  plan: Plan;
  /** The license.key download (an API URL; the server answers with an attachment). */
  downloadHref: string;
  /** More actions after Download and Copy (Email key). */
  actions?: ReactNode;
  title?: string;
}

export function KeyPanel({ token, kid, issuedAt, expiresOn, plan, downloadHref, actions, title = 'License key' }: KeyPanelProps) {
  const notify = useNotify();
  const copy = async () => {
    try {
      // A secure context is needed (https or localhost); plain http on a LAN address has no clipboard.
      if (!navigator.clipboard) throw new Error('This browser does not allow copying here; download the key instead');
      await navigator.clipboard.writeText(token);
      notify.success('Key copied');
    } catch (err) {
      notify.error(err, 'Could not copy the key');
    }
  };
  return (
    <Panel title={title} data-testid="key-panel">
      <CodeBlock code={token} language="plaintext" isWrapped width="100%" title="license.key" size="sm" />
      <HStack gap={2} wrap="wrap" vAlign="center">
        <Button label="Download license.key" variant="primary" icon={<Download size={16} aria-hidden="true" />} href={downloadHref} />
        <Button label="Copy key" variant="secondary" icon={<Copy size={16} aria-hidden="true" />} clickAction={copy} />
        {actions}
      </HStack>
      <Text type="supporting" as="p">
        Signed {utcDay(issuedAt)} with signing key <Text type="code">{kid}</Text>; valid through {utcDay(expiresOn)}.
      </Text>
      <KeyNotices plan={plan} />
    </Panel>
  );
}

export function InstallPanel({ expiresOn }: { expiresOn: string }) {
  return (
    <Panel title="Add the key to kudurru" data-testid="install-panel">
      <Text as="p">Put the key where your nodes read it. A node takes the first of these it finds:</Text>
      <VStack gap={1}>
        {KEY_SOURCES.map((s) => (
          <Text key={s} type="code">
            {s}
          </Text>
        ))}
      </VStack>
      <Text as="p">
        A key file (<Text type="code">KUDURRU_LICENSE_KEY_FILE</Text> or <Text type="code">&lt;node.dataDir&gt;/license.key</Text>) is read again within a minute: replace the file and wait.
      </Text>
      <Text as="p">
        <Text type="code">--license-key</Text> and <Text type="code">KUDURRU_LICENSE_KEY</Text> are read once at start, so restart after changing them: with Helm <Text type="code">kubectl rollout restart</Text>, with Compose{' '}
        <Text type="code">docker compose up -d</Text>.
      </Text>
      <Text as="p">
        Then check that <Text type="code">kudurru license status</Text> shows the new expiry date, {utcDay(expiresOn)}.
      </Text>
    </Panel>
  );
}
