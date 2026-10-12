// ChoiceCards: one choice (a radio group, the wizards' first steps) and
// several (`multiple`, a checkbox group, the list wizard's well-known
// lists), with descriptions, end content, help under the options that
// describes the group, an error, and data-choice on every option.
import { fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { ChoiceCards, type Choice } from './ChoiceCards';

const CHOICES: Choice[] = [
  { value: 'a', label: 'Alpha', description: 'The first one', endContent: <span>MIT</span> },
  { value: 'b', label: 'Beta', description: 'The second one' },
  { value: 'c', label: 'Gamma', isDisabled: true },
];

function Single() {
  const [v, setV] = useState('a');
  return <ChoiceCards label="Kind" value={v} onChange={setV} choices={CHOICES} help="Pick one." />;
}

function Multiple({ error }: { error?: string }) {
  const [v, setV] = useState<string[]>([]);
  return (
    <>
      <ChoiceCards multiple hasDividers label="Lists" value={v} onChange={setV} choices={CHOICES} help="Pick any." error={error} />
      <output>{v.join(',')}</output>
    </>
  );
}

describe('ChoiceCards', () => {
  it('one choice: a radio group with descriptions, help on the group and data-choice', () => {
    render(<Single />);
    const group = screen.getByRole('radiogroup', { name: 'Kind' });
    expect(screen.getByRole('radio', { name: /Alpha/ })).toBeChecked();
    fireEvent.click(screen.getByRole('radio', { name: /Beta/ }));
    expect(screen.getByRole('radio', { name: /Beta/ })).toBeChecked();
    expect(screen.getByRole('radio', { name: /Gamma/ })).toBeDisabled();
    expect(screen.getByText('The first one')).toBeInTheDocument();
    expect(group.getAttribute('aria-describedby') ?? '').not.toBe('');
    expect(document.getElementById((group.getAttribute('aria-describedby') ?? '').split(' ').pop() ?? '')).toHaveTextContent('Pick one.');
    expect(document.querySelector('[data-choice="b"]')).not.toBeNull();
  });

  it('several: a checkbox group that returns every checked value, with end content and an error', () => {
    render(<Multiple error="Choose at least one" />);
    const group = screen.getByRole('group', { name: 'Lists' });
    fireEvent.click(screen.getByRole('checkbox', { name: /Alpha/ }));
    fireEvent.click(screen.getByRole('checkbox', { name: /Beta/ }));
    expect(screen.getByText('a,b')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: /Alpha/ }));
    expect(screen.getByText('b')).toBeInTheDocument();
    expect(screen.getByText('MIT')).toBeInTheDocument();
    expect(screen.getByText('Choose at least one')).toBeInTheDocument();
    expect(group.getAttribute('aria-describedby') ?? '').not.toBe('');
    expect(document.querySelector('[data-choice="a"]')).not.toBeNull();
  });
});
