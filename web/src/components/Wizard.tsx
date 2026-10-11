// A multi-step flow on its own page (docs/m3/ui.md "Pages, wizards and
// dialogs"): an Astryx Stepper across the top, one step's fields at a
// time, and a footer with Back / Next and the finishing action on the last
// step. The caller owns the form state; the wizard only owns which step is
// shown. Next runs the step's `validate` and stays put with its message
// when it returns one. Completed steps (and the current one) are clickable,
// later ones only after every step before them validated, so the review
// step can link back to the step that set a value.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Step, Stepper } from '@astryxdesign/core/Stepper';
import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';

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
  onCancel: () => void;
  /** Shown above the footer, e.g. the API error of the finishing call. */
  error?: ReactNode;
  isFinishing?: boolean;
  /** Start somewhere else than the first step (e.g. ?step= in the URL). */
  initialStep?: number;
  onStepChange?: (index: number) => void;
}

export function Wizard({ label, steps, finishLabel, onFinish, onCancel, error, isFinishing, initialStep = 0, onStepChange }: WizardProps) {
  const [active, setActive] = useState(() => Math.min(Math.max(initialStep, 0), steps.length - 1));
  // The furthest step every earlier step validated for.
  const [reached, setReached] = useState(active);
  const [message, setMessage] = useState<string | null>(null);
  const [checking, setChecking] = useState(false);
  const bodyRef = useRef<HTMLDivElement>(null);
  const last = active === steps.length - 1;
  // steps is never empty; the fallback only satisfies noUncheckedIndexedAccess.
  const step = steps[active] ?? { id: 'none', label: '', content: null };

  const go = (i: number) => {
    setMessage(null);
    setActive(i);
    onStepChange?.(i);
  };

  // Each step change moves focus to the step body so keyboard and screen
  // reader users land on the new fields, not on the button they pressed.
  const first = useRef(true);
  useEffect(() => {
    if (first.current) {
      first.current = false;
      return;
    }
    bodyRef.current?.focus();
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

  const onSubmit = (e: FormEvent) => {
    e.preventDefault();
    void next();
  };

  return (
    <form className="k-wizard" onSubmit={onSubmit} noValidate>
      <Stepper
        activeStep={active}
        label={label}
        onStepClick={(i) => {
          if (i <= reached && i !== active) go(i);
        }}
      >
        {steps.map((s, i) => (
          <Step key={s.id} step={i} label={s.label} description={s.description} isOptional={s.isOptional} isDisabled={i > reached} />
        ))}
      </Stepper>
      <div className="k-wizard-body k-panel" ref={bodyRef} tabIndex={-1} data-step={step.id} aria-label={`Step ${active + 1} of ${steps.length}: ${step.label}`}>
        <h2 className="k-h3">{step.label}</h2>
        {step.description ? <p className="k-muted k-small">{step.description}</p> : null}
        {step.content}
      </div>
      {message ? <Banner status="error" title="Check this step" description={message} /> : null}
      {error}
      <div className="k-wizard-footer">
        <Button label="Cancel" variant="ghost" onClick={onCancel} />
        <span className="k-spacer" />
        {active > 0 ? <Button label="Back" variant="secondary" onClick={() => go(active - 1)} /> : null}
        <Button type="submit" label={last ? finishLabel : 'Next'} variant="primary" isLoading={checking || (last && isFinishing)} />
      </div>
    </form>
  );
}
