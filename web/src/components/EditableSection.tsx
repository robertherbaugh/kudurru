// One section of an object page (docs/m3/ui.md "Pages, wizards and
// dialogs"): a panel with a heading, the current value, and an Edit button
// that swaps in typed controls with their own Save and Cancel. The section
// edits a copy; Save validates it and hands it to onSave, which writes the
// object. Read-only objects keep the Edit button disabled with the reason
// as its tooltip.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { useId, useState, type ReactNode } from 'react';
import { errorText } from './Feedback';

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
}

export function EditableSection<T>({ title, description, value, view, edit, validate, onSave, readOnlyReason, actions, testId }: EditableSectionProps<T>) {
  const id = useId();
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
      setError(errorText(err));
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="k-panel k-section" aria-labelledby={id} data-testid={testId}>
      <div className="k-panel-title">
        <h2 id={id}>{title}</h2>
        <div className="k-row">
          {actions}
          {edit && !editing ? (
            <Button label={`Edit ${title}`} size="sm" variant="secondary" isDisabled={!!readOnlyReason} tooltip={readOnlyReason || undefined} onClick={() => setDraft(structuredClone(value))}>
              Edit
            </Button>
          ) : null}
        </div>
      </div>
      {description ? <p className="k-muted k-small">{description}</p> : null}
      {editing && edit ? (
        <form
          className="k-stack-lg"
          noValidate
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          {edit(draft, (v) => setDraft(v), showErrors)}
          {error ? <Banner status="error" title={`Could not save ${title.toLowerCase()}`} description={error} /> : null}
          <div className="k-row">
            <Button type="submit" label={`Save ${title}`} variant="primary" isLoading={saving}>
              Save
            </Button>
            <Button label="Cancel" variant="ghost" onClick={cancel} />
          </div>
        </form>
      ) : (
        view
      )}
    </section>
  );
}
