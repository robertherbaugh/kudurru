// The Block page section header of Settings (C5): what the block page is,
// the HTTPS caveat, and a Preview of the page as a client would see it for
// a name. The preview HTML comes from GET /blockpage/preview (cookie auth)
// and is shown in a sandboxed iframe through srcdoc: no scripts, no
// same-origin access, and no dependency on the API's frame headers. "Open
// in a new tab" loads the same URL top-level.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { ApiError } from '../api/client';
import { useApi } from '../api/context';
import { hostError } from '../lib/rr';
import { errorText } from './Feedback';
import { FormDialog } from './FormDialog';

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
        refuse to show the page at all. The DNS answer still blocks the site either way.
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
  const [err, setErr] = useState<string>();
  const m = useMutation({ mutationFn: (d: string) => api.blockPage.preview(d) });
  const { mutate } = m;
  // Opening shows the page for the current name at once.
  useEffect(() => {
    if (isOpen) mutate(domain.trim() || 'example.com');
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only on opening
  }, [isOpen, mutate]);
  const show = () => {
    const e = hostError(domain);
    setErr(e ?? undefined);
    if (!e) m.mutate(domain.trim());
  };
  const missing = m.error instanceof ApiError && m.error.status === 404;
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
          <a className="k-small" href={api.blockPage.previewURL(domain.trim() || 'example.com')} target="_blank" rel="noopener noreferrer">
            Open in a new tab
          </a>
          <span className="k-spacer" />
          <Button label="Close" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Show" variant="primary" isLoading={m.isPending} />
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
      {missing ? (
        <Banner status="info" title="No block page on this server" description="This kudurru version has no block page (GET /blockpage/preview answered 404)." />
      ) : m.error ? (
        <Banner status="error" title="The preview could not be loaded" description={errorText(m.error)} />
      ) : null}
      {m.data !== undefined ? (
        <iframe className="k-preview-frame" title={`Block page for ${m.variables ?? domain}`} sandbox="" srcDoc={m.data} referrerPolicy="no-referrer" />
      ) : (
        <p className="k-muted k-small">Choose a name and press Show.</p>
      )}
    </FormDialog>
  );
}
