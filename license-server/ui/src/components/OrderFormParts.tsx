// Parts both Order Form pages show (the admin's /orders/:id and the
// customer's /my/orders/:id): the document preview with its SHA-256 and
// the Download button (the Markdown of shared/orderForms orderFormDocument,
// with both acceptances once there is any), and a signature's rows.
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Download } from 'lucide-react';
import type { OrderFormSignature } from '@shared/api';
import { orderFormFileName } from '@shared/orderForms';
import { utcDateTime } from '../lib/dates';
import { documentText, downloadOrderForm, type DocumentSource } from '../lib/orderForms';
import { Panel } from '../ui/components/layout/Page';

export function DownloadOrderFormButton({ source }: { source: DocumentSource }) {
  return (
    <Button
      label="Download"
      variant="secondary"
      icon={<Download size={16} aria-hidden="true" />}
      tooltip={`Saves ${orderFormFileName(source.number)} (Markdown)`}
      onClick={() => downloadOrderForm(source)}
    />
  );
}

/** The document as it is accepted (or, once signed, kept), with its SHA-256. */
export function DocumentPanel({ source, isDraft }: { source: DocumentSource; isDraft?: boolean }) {
  return (
    <Panel
      title="Document"
      description={
        isDraft
          ? 'The text as it stands. Sending freezes it and records its SHA-256; each party accepts that text.'
          : 'The text each party accepts, identified by its SHA-256. The download is this text, with the acceptances once there are any.'
      }
    >
      <CodeBlock code={documentText(source)} language="markdown" isWrapped width="100%" size="sm" maxHeight="60vh" title={orderFormFileName(source.number)} />
      <VStack gap={0.5}>
        <Text type="supporting">Document SHA-256</Text>
        <Text type="code" data-testid="document-sha256">
          {source.documentSha256 ?? 'Recorded when the form is sent'}
        </Text>
      </VStack>
    </Panel>
  );
}

/** A party's acceptance as metadata rows. */
export function SignatureItem({ label, signature: s, extra }: { label: string; signature: OrderFormSignature | null; extra?: string | null }) {
  return (
    <MetadataListItem label={label}>
      {s ? (
        <VStack gap={0.5}>
          <Text>
            {s.name}, {s.title}
            {s.email ? ` (${s.email})` : ''}
          </Text>
          <Text type="supporting">Accepted electronically on {utcDateTime(s.at)}</Text>
          {extra ? <Text type="supporting">{extra}</Text> : null}
        </VStack>
      ) : (
        <Text color="secondary">Not yet accepted</Text>
      )}
    </MetadataListItem>
  );
}
