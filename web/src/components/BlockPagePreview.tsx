// The Block page section header of Settings (C5): what the block page is,
// the HTTPS caveat with a link to docs/deploy/blocking-page.md, and a
// Preview of the page a client would see for a name. The preview is
// GET /blockpage/preview?domain= (settings.read), which allows framing by
// the dashboard (frame-ancestors 'self'); it is shown in a sandboxed
// iframe (no scripts, no same-origin access) or opened in a new tab.
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Code } from '@astryxdesign/core/Code';
import { Link } from '@astryxdesign/core/Link';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import { useApi } from '../api/context';
import { hostError } from '../lib/rr';
import { TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';

export const BLOCK_PAGE_DOCS = 'https://github.com/robertherbaugh/kudurru/blob/main/docs/deploy/blocking-page.md';

/** The preview fills its Card (which draws the frame and clips the corners). */
const FRAME_STYLE = { display: 'block', inlineSize: '100%', blockSize: '100%', border: 'none' } as const;

export function BlockPageIntro() {
  const [open, setOpen] = useState(false);
  return (
    <VStack gap={1.5} data-testid="blockpage-intro">
      <Text size="sm" as="p">
        With the{' '}
        <Text type="inherit" weight="semibold">
          Block page
        </Text>{' '}
        response (blocking.defaultResponse, or a policy's or list's block mode), blocked names answer with this
        server's block page address, so a browser opening a blocked site sees which list, rule or policy blocked it and whom to ask, instead of a
        connection error.
      </Text>
      <Text type="supporting" as="p">
        Plain <Code>http://</Code> pages show it directly. For <Code>https://</Code> sites the browser expects a certificate for the blocked domain, which
        the block page cannot have: it shows a certificate warning, and sites with HSTS (most large ones, and every name on the browsers' preload list)
        refuse to show the page at all. The DNS answer still blocks the site either way.{' '}
        <Link href={BLOCK_PAGE_DOCS} target="_blank" type="inherit" hasUnderline>
          Block page deployment notes
        </Link>
      </Text>
      <HStack gap={2} vAlign="center">
        <Button label="Preview" size="sm" variant="secondary" onClick={() => setOpen(true)} />
      </HStack>
      <BlockPagePreviewDialog isOpen={open} onClose={() => setOpen(false)} />
    </VStack>
  );
}

export function BlockPagePreviewDialog({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  const api = useApi();
  const [domain, setDomain] = useState('ads.example.com');
  const [shown, setShown] = useState('ads.example.com');
  const [err, setErr] = useState<string>();
  const show = () => {
    const e = hostError(domain);
    setErr(e ?? undefined);
    if (!e) setShown(domain.trim());
  };
  return (
    <FormDialog
      isOpen={isOpen}
      onClose={onClose}
      title="Block page preview"
      width={860}
      noValidate
      onSubmit={show}
      footer={
        <>
          <Link href={api.blockPage.previewURL(shown)} target="_blank" size="sm" hasUnderline>
            Open in a new tab
          </Link>
          <StackItem size="fill" />
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Show" variant="primary" />
        </>
      }
    >
      <TextField
        label="Blocked domain"
        help="The page shows this name as the blocked site"
        value={domain}
        onChange={setDomain}
        placeholder="ads.example.com"
        mono
        error={err}
      />
      <Card padding={0} height="min(60dvh, 560px)">
        <iframe key={shown} style={FRAME_STYLE} title={`Block page for ${shown}`} sandbox="" src={api.blockPage.previewURL(shown)} referrerPolicy="no-referrer" />
      </Card>
      <Text type="supporting" as="p">
        Rendered by this server with the current block page settings; the reason shown is a sample.
      </Text>
    </FormDialog>
  );
}
