// "View as YAML": the object as objects.d or `kubectl apply` takes it
// (status and server-set metadata left out), in a dialog with a copy
// button. The escape hatch of every object page.
import { Button } from '@astryxdesign/core/Button';
import { useState } from 'react';
import type { AnyObject } from '../api/types';
import { toYAML } from '../lib/yaml';
import { FormDialog } from './FormDialog';

/** The object without status and the metadata the server owns. */
export function manifestOf(obj: AnyObject): Record<string, unknown> {
  const { uid: _uid, resourceVersion: _rv, generation: _gen, creationTimestamp: _ts, ...metadata } = obj.metadata;
  return { apiVersion: obj.apiVersion, kind: obj.kind, metadata, spec: obj.spec };
}

export function YamlButton({ obj }: { obj: AnyObject }) {
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const text = toYAML(manifestOf(obj));
  return (
    <>
      <Button label="View as YAML" variant="ghost" onClick={() => setOpen(true)} />
      <FormDialog
        isOpen={open}
        onClose={() => {
          setOpen(false);
          setCopied(false);
        }}
        title={`${obj.kind} ${obj.metadata.name} as YAML`}
        width={720}
        footer={
          <>
            <Button label={copied ? 'Copied' : 'Copy'} variant="secondary" onClick={() => void navigator.clipboard?.writeText(text).then(() => setCopied(true))} />
            <Button label="Close" variant="primary" onClick={() => setOpen(false)} />
          </>
        }
      >
        <pre className="k-pre" data-testid="object-yaml">
          {text}
        </pre>
        <p className="k-muted k-small">Apply it on the Tools page, with kudurru apply -f, or put it into objects.d.</p>
      </FormDialog>
    </>
  );
}
