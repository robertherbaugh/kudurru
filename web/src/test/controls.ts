// Driving the Astryx controls in tests. A Selector or MultiSelector is a
// combobox button whose options live in the listbox it controls (always in
// the DOM, shown in a popover); SelectField puts the value on the trigger
// as data-value. NumberInput keeps typed text as a draft until Enter or
// blur; Typeahead searches after a debounce and commits on a pick.
import { act, fireEvent, waitFor, within } from '@testing-library/react';
import { expect } from 'vitest';

/**
 * A Selector's trigger by its label: a combobox, or a button opening a
 * listbox when the Selector has a search box (hasSearch).
 */
export function getSelector(name: string | RegExp, root: HTMLElement = document.body): HTMLElement {
  const found = [
    ...within(root).queryAllByRole('combobox', { name }),
    ...within(root)
      .queryAllByRole('button', { name })
      .filter((b) => b.getAttribute('aria-haspopup') === 'listbox'),
  ];
  if (!found[0]) throw new Error(`no selector named ${String(name)}`);
  return found[0];
}

/** Like getSelector, waiting for it to appear. */
export async function findSelector(name: string | RegExp, root: HTMLElement = document.body): Promise<HTMLElement> {
  let el: HTMLElement | undefined;
  await waitFor(() => {
    el = getSelector(name, root);
  });
  return el as HTMLElement;
}

/** The listbox a Selector/MultiSelector trigger controls (opening it when the listbox renders only while open). */
export function listboxOf(combo: HTMLElement): HTMLElement {
  const id = combo.getAttribute('aria-controls');
  let el = id ? document.getElementById(id) : null;
  if (!el) {
    fireEvent.click(combo);
    const id2 = combo.getAttribute('aria-controls');
    el = id2 ? document.getElementById(id2) : null;
  }
  if (!el) throw new Error(`no listbox for ${combo.outerHTML.slice(0, 120)}`);
  return el;
}

/** The option elements of a Selector/MultiSelector. */
export function optionsOf(combo: HTMLElement): HTMLElement[] {
  return within(listboxOf(combo)).queryAllByRole('option', { hidden: true });
}

/** The labels of a Selector's options, in order. */
export function optionLabels(combo: HTMLElement): string[] {
  return optionsOf(combo).map((o) => (o.querySelector('.astryx-selector-option span span')?.textContent ?? o.textContent ?? '').trim());
}

/** Opens the Selector and clicks the option with this accessible name (string = exact). */
export function pick(combo: HTMLElement, option: string | RegExp): void {
  if (combo.getAttribute('aria-expanded') !== 'true') fireEvent.click(combo);
  const opt = within(listboxOf(combo)).getAllByRole('option', { hidden: true, name: option })[0];
  if (!opt) throw new Error(`no option ${String(option)}`);
  fireEvent.click(opt);
}

/** The value a SelectField shows (its trigger's data-value). */
export function valueOf(combo: HTMLElement): string | null {
  return combo.getAttribute('data-value');
}

/** Types into a NumberInput (NumberField reports each input event) and commits the draft (blur). */
export function typeNumber(input: HTMLElement, text: string, opts: { blur?: boolean } = {}): void {
  fireEvent.focus(input);
  fireEvent.input(input, { target: { value: text } });
  if (opts.blur !== false) fireEvent.blur(input);
}

/** Types into a Typeahead and commits the typed text with Enter once the results are in. */
export async function typeAhead(input: HTMLElement, text: string): Promise<void> {
  fireEvent.focus(input);
  fireEvent.change(input, { target: { value: text } });
  await waitFor(() => expect(input).toHaveAttribute('aria-expanded', 'true'));
  await act(async () => {
    fireEvent.keyDown(input, { key: 'Enter' });
    await Promise.resolve();
  });
}
