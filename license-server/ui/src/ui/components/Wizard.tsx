// Copied from dashboard/src/components/Wizard.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// A multi-step creation flow in a modal over the page the user is on
// (dev-docs/milestones/m3/ui.md "Pages, wizards and dialogs"), composed like Astryx's
// form-wizard-dialog template: a FormDialog whose Layout pins the dialog
// header and a compact Astryx Stepper above the scrolling step body, and
// pins the footer (Cancel, the step's message, Back, Next or the finishing
// action) below it, so every button stays in view on a short screen.
//
// WizardDialog owns the dialog: ✕, Escape and Cancel close it, and once
// anything was entered (an input or change event inside, or the caller's
// isDirty) they ask "Discard the new …?" first; the backdrop never closes a
// dirty wizard. The caller renders its Wizard as the dialog's child, so the
// caller's form state lives in a component that mounts with each opening
// and starts clean every time.
//
// Wizard owns which step is shown. Next runs the step's `validate` and
// stays put with its message when it returns one. Completed steps (and the
// current one) are clickable, later ones only after every step before them
// validated, so the review step can link back to the step that set a value
// (useWizardNav inside a step's content). Each step change moves focus to
// the step's heading; an optional step says so beside it (the compact
// Stepper has no room for "Optional" under six labels).
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Button } from '@astryxdesign/core/Button';
import { DialogHeader } from '@astryxdesign/core/Dialog';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Heading } from '@astryxdesign/core/Heading';
import { Layout, LayoutContent, LayoutFooter, LayoutHeader } from '@astryxdesign/core/Layout';
import { MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Step, Stepper } from '@astryxdesign/core/Stepper';
import { Text } from '@astryxdesign/core/Text';
import { createContext, useContext, useEffect, useId, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { FormDialog, useDialogFrame } from './FormDialog';

export interface WizardStep {
  /** Stable key (also the `data-step` attribute of the step body). */
  id: string;
  label: string;
  description?: string;
  isOptional?: boolean;
  content: ReactNode;
  /** A message blocks Next; null or undefined lets it through. */
  validate?: () => string | null | undefined | Promise<string | null | undefined>;
}

export interface WizardProps {
  /** Accessible name of the step list, e.g. "New policy". */
  label: string;
  steps: WizardStep[];
  /** The last step's button ("Create policy"). */
  finishLabel: string;
  onFinish: () => void | Promise<void>;
  /** Cancel; inside a WizardDialog it defaults to closing it (with the discard question). */
  onCancel?: () => void;
  /** Whether anything was entered; input and change events inside count too. */
  isDirty?: boolean;
  /** Above the first step's fields, e.g. a read-only banner. */
  notice?: ReactNode;
  /** Above the footer's buttons, e.g. the API error of the finishing call. */
  error?: ReactNode;
  isFinishing?: boolean;
  /** Start somewhere else than the first step. */
  initialStep?: number;
  onStepChange?: (index: number) => void;
}

interface WizardNav {
  /** Shows a step (by id or index) the user already reached. */
  goTo: (step: string | number) => void;
}

const NavContext = createContext<WizardNav>({ goTo: () => undefined });

/** Step navigation for a step's content, e.g. the review step's "Edit" links. */
export function useWizardNav(): WizardNav {
  return useContext(NavContext);
}

export interface ReviewRowProps {
  label: string;
  /** The step (id) that sets the value. */
  step: string;
  /** For the Edit button's name: "Edit <what>". */
  what: string;
  children: ReactNode;
}

/** A review step's row (inside a MetadataList): the value, and Edit back to its step. */
export function ReviewRow({ label, step, what, children }: ReviewRowProps) {
  const nav = useWizardNav();
  return (
    <MetadataListItem label={label}>
      <HStack gap={2} vAlign="center">
        <StackItem size="fill">{children}</StackItem>
        <Button label={`Edit ${what}`} size="sm" variant="ghost" onClick={() => nav.goTo(step)}>
          Edit
        </Button>
      </HStack>
    </MetadataListItem>
  );
}

export interface WizardDialogProps {
  isOpen: boolean;
  /** Closes without asking (after a discard, a finished flow, or a pristine wizard). */
  onClose: () => void;
  /** "New policy". */
  title: string;
  subtitle?: ReactNode;
  /** What is created, for "Discard the new policy?". */
  what: string;
  /** Preferred width in pixels (the dialog never gets wider than the viewport). */
  width?: number;
  /** The caller's wizard: a component rendering <Wizard>, mounted while open. */
  children: ReactNode;
}

export function WizardDialog({ isOpen, onClose, title, subtitle, what, width = 820, children }: WizardDialogProps) {
  const [dirty, setDirty] = useState(false);
  const [confirming, setConfirming] = useState(false);
  // Every opening starts pristine (state adjusted during render, as FormDialog does).
  const [wasOpen, setWasOpen] = useState(isOpen);
  if (wasOpen !== isOpen) {
    setWasOpen(isOpen);
    if (!isOpen) {
      setDirty(false);
      setConfirming(false);
    }
  }
  const requestClose = () => {
    if (dirty) setConfirming(true);
    else onClose();
  };
  return (
    <>
      <FormDialog isOpen={isOpen} onClose={requestClose} title={title} subtitle={subtitle} width={width} isDirty={dirty} onDirty={() => setDirty(true)}>
        {children}
      </FormDialog>
      <AlertDialog
        isOpen={isOpen && confirming}
        onOpenChange={(open) => {
          if (!open) setConfirming(false);
        }}
        title={`Discard the new ${what}?`}
        description="What you entered in this wizard is lost."
        cancelLabel="Keep editing"
        actionLabel="Discard"
        onAction={() => {
          setConfirming(false);
          onClose();
        }}
      />
    </>
  );
}

export function Wizard({ label, steps, finishLabel, onFinish, onCancel, isDirty, notice, error, isFinishing, initialStep = 0, onStepChange }: WizardProps) {
  const frame = useDialogFrame();
  const [active, setActive] = useState(() => Math.min(Math.max(initialStep, 0), steps.length - 1));
  // The furthest step every earlier step validated for.
  const [reached, setReached] = useState(active);
  const [message, setMessage] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const headingId = useId();
  const last = active === steps.length - 1;
  // steps is never empty; the fallback only satisfies noUncheckedIndexedAccess.
  const step = steps[active] ?? { id: 'none', label: '', content: null };
  const cancel = onCancel ?? frame?.onClose;

  // The caller knows about changes no input event reports (a choice card, an added row).
  const markDirty = frame?.markDirty;
  useEffect(() => {
    if (isDirty) markDirty?.();
    // markDirty is a new function on every render of the frame.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isDirty]);

  const go = (i: number) => {
    setMessage(null);
    setActive(i);
    onStepChange?.(i);
  };

  // Each step change moves focus to the step's heading so keyboard and
  // screen reader users land on the new step, not on the button they
  // pressed. The first step keeps the dialog title's focus on open.
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    headingRef.current?.focus();
  }, [active]);

  const validateCurrent = async (): Promise<boolean> => {
    if (!step.validate) return true;
    setChecking(true);
    try {
      const msg = await step.validate();
      setMessage(msg ?? null);
      return !msg;
    } finally {
      setChecking(false);
    }
  };

  const next = async () => {
    if (!(await validateCurrent())) return;
    if (last) {
      await onFinish();
      return;
    }
    setReached((r) => Math.max(r, active + 1));
    go(active + 1);
  };

  const nav: WizardNav = {
    goTo: (target) => {
      const i = typeof target === 'number' ? target : steps.findIndex((s) => s.id === target);
      if (i >= 0 && i <= reached && i !== active) go(i);
    },
  };

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    void next();
  };

  return (
    // The form fills the dialog (a flex column) so the Layout's content scrolls and its footer stays in view.
    <StackItem as="form" size="fill" {...formAttrs} onSubmit={onSubmit} onInput={markDirty} onChange={markDirty} aria-label={label}>
      <Layout
        height="fill"
        padding={4}
        defaultHasDividers
        header={
          <>
            <DialogHeader title={frame?.title ?? label} subtitle={frame?.subtitle} onOpenChange={frame?.onClose ? (open) => !open && frame.onClose?.() : undefined} hasDivider={false} />
            <LayoutHeader>
              <Stepper
                activeStep={active}
                label={label}
                density="compact"
                onStepClick={(i) => {
                  if (i <= reached && i !== active) go(i);
                }}
              >
                {steps.map((s, i) => (
                  <Step key={s.id} step={i} label={s.label} isDisabled={i > reached} />
                ))}
              </Stepper>
            </LayoutHeader>
          </>
        }
        content={
          <LayoutContent>
            <VStack gap={4} paddingBlockStart={2} role="group" aria-labelledby={headingId} data-step={step.id}>
              <VStack gap={1}>
                <HStack gap={2} vAlign="center">
                  <Heading level={3} id={headingId} ref={headingRef} tabIndex={-1}>
                    {step.label}
                  </Heading>
                  {step.isOptional ? <Text type="supporting">Optional</Text> : null}
                </HStack>
                {step.description ? (
                  <Text type="supporting" display="block">
                    {step.description}
                  </Text>
                ) : null}
              </VStack>
              {active === 0 ? notice : null}
              <NavContext value={nav}>{step.content}</NavContext>
            </VStack>
          </LayoutContent>
        }
        footer={
          <LayoutFooter>
            <VStack gap={2}>
              {error}
              <HStack gap={2} vAlign="center">
                {cancel ? <Button label="Cancel" variant="ghost" onClick={cancel} /> : null}
                <StackItem size="fill">{message ? <FieldStatus type="error" variant="detached" message={message} /> : null}</StackItem>
                {active > 0 ? <Button label="Back" variant="secondary" onClick={() => go(active - 1)} /> : null}
                <Button type="submit" label={last ? finishLabel : 'Next'} variant="primary" isLoading={checking || (last && isFinishing)} />
              </HStack>
            </VStack>
          </LayoutFooter>
        }
      />
    </StackItem>
  );
}

/** <form> attributes the Astryx stack props do not type (they reach the element). */
const formAttrs: object = { noValidate: true };
