// Copied from dashboard/src/components/fields/FieldHelp.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// Helper text under the control (the dashboard rule: never between the
// label and the control, so a row of fields lines up on its inputs).
// Astryx's `description` always renders between label and control, in
// every FormLayout direction, and has no placement option, so the field
// wrappers pass no description: they render the Astryx control with its
// own label, then the help as supporting Text, then the error as a
// detached FieldStatus. TextInput, TextArea, NumberInput, Selector,
// MultiSelector, Typeahead, TimeInput and DateTimeInput compute
// aria-describedby themselves and overwrite a caller's value (only
// CheckboxInput merges it), so useDescribedBy adds the help and error ids
// to the control the field's label points at after every render and again
// whenever the control rewrites the attribute.
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useId, useLayoutEffect, useRef, type ReactNode } from 'react';

const ADDED = 'data-k-describedby';

/** Which element a field describes: the labeled control, or the group (CheckboxList, RadioList). */
export type Described = 'control' | 'group';

/** The control a field wrapper labels: the element its <label for> names, else the first group or input inside. */
function controlOf(root: HTMLElement, what: Described): HTMLElement | null {
  if (what === 'group') {
    const g = root.querySelector<HTMLElement>('[role="group"], [role="radiogroup"]');
    if (g) return g;
  }
  const label = root.querySelector<HTMLLabelElement>('label[for]');
  if (label?.htmlFor) {
    const el = root.ownerDocument.getElementById(label.htmlFor);
    if (el && root.contains(el)) return el;
  }
  return root.querySelector<HTMLElement>('[role="radiogroup"], [role="group"], [role="combobox"], input, textarea');
}

function link(el: HTMLElement, ids: string[]) {
  const before = el.getAttribute('aria-describedby') ?? '';
  const prev = (el.getAttribute(ADDED) ?? '').split(' ').filter(Boolean);
  const own = before.split(/\s+/).filter((x) => x && !prev.includes(x));
  const next = [...own, ...ids.filter((x) => !own.includes(x))].join(' ');
  if (next !== before) {
    if (next) el.setAttribute('aria-describedby', next);
    else el.removeAttribute('aria-describedby');
  }
  if (ids.length) el.setAttribute(ADDED, ids.join(' '));
  else el.removeAttribute(ADDED);
}

/** A ref for a field's root; the control inside is described by `ids` (falsy entries skipped). */
export function useDescribedBy<T extends HTMLElement = HTMLElement>(ids: (string | false | null | undefined)[], what: Described = 'control') {
  const ref = useRef<T>(null);
  const want = ids.filter((x): x is string => Boolean(x)).join(' ');
  useLayoutEffect(() => {
    const root = ref.current;
    const el = root ? controlOf(root, what) : null;
    if (!el) return;
    const list = want ? want.split(' ') : [];
    link(el, list);
    const mo = new MutationObserver(() => link(el, list));
    mo.observe(el, { attributes: true, attributeFilter: ['aria-describedby'] });
    return () => mo.disconnect();
  });
  return ref;
}

export interface HelpParts {
  /** Help under the control. */
  help?: ReactNode;
  /** An error under the help (also marks the control invalid). */
  error?: string | undefined;
  /** A warning under the help, when there is no error. */
  warning?: string | undefined;
}

/** Ids and the rendered help/status for a field wrapper. */
export function useFieldHelp({ help, error, warning }: HelpParts, what: Described = 'control') {
  const id = useId();
  const helpId = help ? `${id}-help` : undefined;
  const statusId = error || warning ? `${id}-status` : undefined;
  const ref = useDescribedBy<HTMLElement>([helpId, statusId], what);
  const status = error ? ({ type: 'error' } as const) : warning ? ({ type: 'warning' } as const) : undefined;
  const foot = (
    <>
      {help ? (
        <Text type="supporting" display="block" id={helpId}>
          {help}
        </Text>
      ) : null}
      {error || warning ? <FieldStatus type={error ? 'error' : 'warning'} message={(error ?? warning) as string} id={statusId} variant="detached" /> : null}
    </>
  );
  return { ref, status, foot };
}

export interface FieldShellProps extends HelpParts {
  children: (status: { type: 'error' | 'warning' } | undefined) => ReactNode;
  /** Stretch to the column (default) or hug the control. */
  width?: string | number | undefined;
  'data-testid'?: string | undefined;
  /** Describe the group instead of the labeled control. */
  describes?: Described;
}

/** label+control (from children) → help → status, the help linked to the control. */
export function FieldShell({ help, error, warning, children, width, 'data-testid': testId, describes }: FieldShellProps) {
  const { ref, status, foot } = useFieldHelp({ help, error, warning }, describes);
  return (
    <VStack ref={ref} gap={1} width={width} data-testid={testId}>
      {children(status)}
      {foot}
    </VStack>
  );
}
