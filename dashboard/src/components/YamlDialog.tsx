// "View as YAML": an object as `kudurru apply -f` and kubectl take it,
// without the server-owned status and bookkeeping metadata. Read-only, with
// copy; changes go through the page or `kudurru apply`.
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Text } from '@astryxdesign/core/Text';
import type { AnyObject } from '../api/types';
import { toYAML } from '../lib/yaml';
import { useNotify } from './Feedback';
import { FormDialog } from './FormDialog';

/** The object as it would be applied: no status, uid, resourceVersion, generation or creation time. */
export function applyable(obj: AnyObject): Record<string, unknown> {
  const { uid: _u, resourceVersion: _r, generation: _g, creationTimestamp: _c, ...metadata } = obj.metadata;
  return { apiVersion: obj.apiVersion, kind: obj.kind, metadata, spec: obj.spec };
}

export function YamlDialog({ obj, open, onClose }: { obj: AnyObject | undefined; open: boolean; onClose: () => void }) {
  const notify = useNotify();
  const text = obj ? toYAML(applyable(obj)) : '';
  return (
    <FormDialog
      isOpen={open}
      onClose={onClose}
      title={obj ? `${obj.kind} ${obj.metadata.name} as YAML` : 'YAML'}
      width={760}
      footer={
        <>
          <Button
            label="Copy"
            variant="secondary"
            onClick={() => {
              void navigator.clipboard?.writeText(text).then(
                () => notify.success('Copied the YAML'),
                (err: unknown) => notify.error(err, 'Could not copy'),
              );
            }}
          />
          <Button label="Close" variant="primary" onClick={onClose} />
        </>
      }
    >
      {/* The footer has the Copy button; no language header (the dialog title says YAML). */}
      <CodeBlock aria-label="YAML" code={text} language="yaml" hasLanguageLabel={false} hasCopyButton={false} maxHeight={480} width="100%" />
      <Text type="supporting" as="p">
        Apply a changed copy with kudurru apply -f, or keep it in objects.d / Git.
      </Text>
    </FormDialog>
  );
}
