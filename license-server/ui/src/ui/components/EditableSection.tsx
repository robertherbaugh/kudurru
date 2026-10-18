// Copied from dashboard/src/components/EditableSection.tsx at dd3dcb0; keep in step by hand.
// Changes: an optional describeError, so a page can word a refused save
// without the server's field paths (api/fieldErrors bannerError).
// One section of an object page (dev-docs/milestones/m3/ui.md "Pages, wizards and
// dialogs"): a Panel (a region named by its heading) with the current
// value, and an Edit button that swaps in typed controls with their own
// Save and Cancel. The section
// edits a copy; Save validates it and hands it to onSave, which writes the
// object. Read-only objects keep the Edit button disabled with the reason
// as its tooltip.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { useState, type FormEvent, type ReactNode } from 'react';
import { errorText } from './Feedback';
import { Panel } from './layout/Page';

export interface EditableSectionProps<T> {
  title: string;
  description?: ReactNode;
  value: T;
  /** Shown while not editing. */
  view: ReactNode;
  /** The controls; showErrors is true after a failed Save. */
  edit?: (draft: T, setDraft: (v: T) => void, showErrors: boolean) => ReactNode;
  validate?: (draft: T) => string | null;
  onSave?: (draft: T) => Promise<void>;
  /** Why editing is not possible (read-only object, missing permission). */
  readOnlyReason?: string;
  /** Extra header actions (links). */
  actions?: ReactNode;
  testId?: string;
  /** The banner text for a failed onSave (default errorText); return null when the controls already show it all. */
  describeError?: (err: unknown) => string | null;
}

export function EditableSection<T>({ title, description, value, view, edit, validate, onSave, readOnlyReason, actions, testId, describeError }: EditableSectionProps<T>) {
  const [draft, setDraft] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const [saving, setSaving] = useState(false);
  const editing = draft !== null;

  const cancel = () => {
    setDraft(null);
    setError(null);
    setShowErrors(false);
  };
  const save = async () => {
    if (draft === null || !onSave) return;
    const msg = validate?.(draft) ?? null;
    if (msg) {
      setError(msg);
      setShowErrors(true);
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await onSave(draft);
      cancel();
    } catch (err) {
      setError(describeError ? describeError(err) : errorText(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Panel
      title={title}
      description={description}
      data-testid={testId}
      actions={
        actions || (edit && !editing) ? (
          <>
            {actions}
            {edit && !editing ? (
              <Button label={`Edit ${title}`} size="sm" variant="secondary" isDisabled={!!readOnlyReason} tooltip={readOnlyReason || undefined} onClick={() => setDraft(structuredClone(value))}>
                Edit
              </Button>
            ) : null}
          </>
        ) : undefined
      }
    >
      {editing && edit ? (
        <VStack
          as="form"
          gap={4}
          {...formAttrs}
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            void save();
          }}
        >
          {edit(draft, (v) => setDraft(v), showErrors)}
          {error ? <Banner status="error" title={`Could not save ${title.toLowerCase()}`} description={error} /> : null}
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Button type="submit" label={`Save ${title}`} variant="primary" isLoading={saving}>
              Save
            </Button>
            <Button label="Cancel" variant="ghost" onClick={cancel} />
          </HStack>
        </VStack>
      ) : (
        view
      )}
    </Panel>
  );
}

/** <form> attributes the Astryx stack props do not type (they reach the element). */
const formAttrs: object = { noValidate: true };
