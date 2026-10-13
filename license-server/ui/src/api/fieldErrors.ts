// The server's 400 `error.fields` next to the controls they name
// (docs/m3/ui.md: "A 400 names the failing parts of a value inline").
// A form lists the body fields its controls show; each control asks for its
// message (fieldError, or the hook's `field`), and the form's banner only
// carries what no control shows (bannerError), so a wire path such as
// "newCustomer.contactEmail" never reaches the page. The body validator's
// own message lists those paths, so under a control it becomes a plain
// "Missing or not valid"; a service's sentence ("A renewal must expire
// after …") is shown as it is. useFieldErrors drops a control's message
// once the person edits it, until the next answer, and gives the banner a
// pointer to the marked controls when they carry the whole refusal.
import { useCallback, useState } from 'react';
import { ApiError } from './client';

/** What server/http/body.ts says when it refuses fields: the paths follow the colon. */
const VALIDATOR_PREFIX = 'Some values are missing or not valid';

/** Under a control, for the body validator's refusal. */
export const NOT_VALID = 'Missing or not valid';

/** Under each control of a refusal that names several of them (the sentences stay in the banner). */
export const SEE_BELOW = 'Not accepted: see the message below';

/** A 400 that names body fields. */
function fieldsError(err: unknown): ApiError | null {
  return err instanceof ApiError && err.status === 400 && err.fields.length > 0 ? err : null;
}

function fromValidator(err: ApiError): boolean {
  return err.message.startsWith(VALIDATOR_PREFIX);
}

/** Whether a reported field (a dotted path) belongs to one of the controls' fields. */
function covers(handled: readonly string[], field: string): boolean {
  return handled.some((h) => field === h || field.startsWith(`${h}.`));
}

/** The message for the control that shows these body fields, or undefined. */
export function fieldError(err: unknown, ...fields: string[]): string | undefined {
  const e = fieldsError(err);
  if (!e || !fields.some((f) => e.hasField(f))) return undefined;
  if (fromValidator(e)) return NOT_VALID;
  return e.fields.length === 1 ? e.message : SEE_BELOW;
}

/**
 * The form banner's text: null when there is no error or when the controls
 * (`handled`, the body fields they show) already say everything, else a
 * sentence without wire paths.
 */
export function bannerError(err: unknown, handled: readonly string[]): string | null {
  if (err === null || err === undefined) return null;
  const e = fieldsError(err);
  if (e) {
    const all = e.fields.every((f) => covers(handled, f));
    if (fromValidator(e)) return all ? null : `${VALIDATOR_PREFIX}.`;
    if (all && e.fields.length === 1) return null;
    return e.message;
  }
  if (err instanceof ApiError) return err.describe();
  if (err instanceof Error) return err.message;
  return typeof err === 'string' ? err : 'Something went wrong.';
}

/** The first of `order` (body fields in the order the form asks for them) that a 400 names. */
export function firstField(err: unknown, order: readonly string[]): string | undefined {
  const e = fieldsError(err);
  return e ? order.find((f) => e.hasField(f)) : undefined;
}

/** Whether a 400 names only fields the form's controls show. */
export function isFieldError(err: unknown, handled: readonly string[]): boolean {
  const e = fieldsError(err);
  return e !== null && e.fields.every((f) => covers(handled, f));
}

export interface FieldErrors {
  /** The server's message for the control showing these body fields; gone once it was edited. */
  field: (...fields: string[]) => string | undefined;
  /** Call from a control's onChange: its server message goes away until the next answer. */
  edited: (field: string) => void;
  /**
   * The form banner's description, null for none: what no control shows
   * (bannerError), else, while a control still shows the server's message,
   * MARKED, so a refusal never goes unannounced (the banner is an alert).
   */
  summary: string | null;
}

/** The banner's description when the controls show the whole refusal. */
export const MARKED = 'The portal did not accept the marked values.';

/** fieldError and bannerError for a mutation's error, with the edited controls' messages dropped. */
export function useFieldErrors(err: unknown, handled: readonly string[]): FieldErrors {
  const [edited, setEdited] = useState<{ err: unknown; fields: readonly string[] }>({ err: null, fields: [] });
  const done = edited.err === err ? edited.fields : [];
  const markEdited = useCallback(
    (field: string) =>
      setEdited((prev) => {
        const fields = prev.err === err ? prev.fields : [];
        return fields.includes(field) ? prev : { err, fields: [...fields, field] };
      }),
    [err],
  );
  const field = (...fields: string[]) => (fields.some((f) => done.includes(f)) ? undefined : fieldError(err, ...fields));
  const banner = bannerError(err, handled);
  const marked = handled.some((f) => field(f) !== undefined);
  return { field, edited: markEdited, summary: banner ?? (marked ? MARKED : null) };
}
