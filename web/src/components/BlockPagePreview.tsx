// The Block page section header of Settings (C5): what the block page is,
// the HTTPS caveat with a link to docs/deploy/blocking-page.md, and a
// Preview of the page a client would see for a name. The preview is
// GET /blockpage/preview?domain= (settings.read), which allows framing by
// the dashboard (frame-ancestors 'self'); it is shown in a sandboxed
// iframe (no scripts, no same-origin access) or opened in a new tab.
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useState } from 'react';
import { useApi } from '../api/context';
import { hostError } from '../lib/rr';
import { FormDialog } from './FormDialog';

export const BLOCK_PAGE_DOCS = 'https://github.com/robertherbaugh/kudurru/blob/main/docs/deploy/blocking-page.md';

export function BlockPageIntro() {
  const [open, setOpen] = useState(false);
  return (
    <div className="k-stack-sm" data-testid="blockpage-intro">
      <p className="k-small">
        With the <strong>Block page</strong> response (blocking.defaultResponse, or a policy's or list's block mode), blocked names answer with this
        server's block page address, so a browser opening a blocked site sees which list, rule or policy blocked it and whom to ask, instead of a
        connection error.
      </p>
      <p className="k-small k-muted">
        Plain <code>http://</code> pages show it directly. For <code>https://</code> sites the browser expects a certificate for the blocked domain, which
        the block page cannot have: it shows a certificate warning, and sites with HSTS (most large ones, and every name on the browsers' preload list)
        refuse to show the page at all. The DNS answer still blocks the site either way.{' '}
        <a href={BLOCK_PAGE_DOCS} target="_blank" rel="noopener noreferrer">
          Block page deployment notes
        </a>
      </p>
      <span className="k-row">
        <Button label="Preview" size="sm" variant="secondary" onClick={() => setOpen(true)} />
      </span>
      <BlockPagePreviewDialog isOpen={open} onClose={() => setOpen(false)} />
    </div>
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
          <a className="k-small" href={api.blockPage.previewURL(shown)} target="_blank" rel="noopener noreferrer">
            Open in a new tab
          </a>
          <span className="k-spacer" />
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Show" variant="primary" />
        </>
      }
    >
      <TextInput
        label="Blocked domain"
        description="The page shows this name as the blocked site"
        value={domain}
        onChange={setDomain}
        placeholder="ads.example.com"
        status={err ? { type: 'error', message: err } : undefined}
      />
      <iframe key={shown} className="k-preview-frame" title={`Block page for ${shown}`} sandbox="" src={api.blockPage.previewURL(shown)} referrerPolicy="no-referrer" />
      <p className="k-muted k-small">Rendered by this server with the current block page settings; the reason shown is a sample.</p>
    </FormDialog>
  );
}
