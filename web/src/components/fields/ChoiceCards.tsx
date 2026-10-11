// A closed choice shown as cards, for the first step of a wizard where
// each option needs a sentence (zone type, upstream provider): native
// radio buttons in a fieldset, so arrows move between cards and the
// choice is announced like any radio group.
import { useId, type ReactNode } from 'react';

export interface Choice {
  value: string;
  label: string;
  description?: ReactNode;
  /** Shown before the label (a provider icon). */
  icon?: ReactNode;
  isDisabled?: boolean;
}

export interface ChoiceCardsProps {
  label: string;
  isLabelHidden?: boolean;
  value: string;
  choices: Choice[];
  onChange: (v: string) => void;
  error?: string | undefined;
}

export function ChoiceCards({ label, isLabelHidden, value, choices, onChange, error }: ChoiceCardsProps) {
  const name = useId();
  return (
    <fieldset className="k-choice-grid" aria-invalid={error ? true : undefined}>
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>{label}</legend>
      {choices.map((c) => (
        <label key={c.value} className="k-choice" data-choice={c.value}>
          <input type="radio" name={name} value={c.value} checked={value === c.value} disabled={c.isDisabled} onChange={() => onChange(c.value)} />
          {c.icon}
          <span>
            <strong>{c.label}</strong>
            {c.description ? <span className="k-muted k-small">{c.description}</span> : null}
          </span>
        </label>
      ))}
      {error ? <span className="k-error-text">{error}</span> : null}
    </fieldset>
  );
}
