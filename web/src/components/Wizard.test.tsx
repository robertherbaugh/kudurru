import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { Wizard, useWizardNav, type WizardStep } from './Wizard';

function steps(validate?: () => string | null): WizardStep[] {
  return [
    { id: 'who', label: 'Who', content: <p>who fields</p>, validate },
    { id: 'what', label: 'What', content: <p>what fields</p> },
    { id: 'review', label: 'Review', content: <p>review</p> },
  ];
}

describe('Wizard', () => {
  it('blocks Next with the step message and finishes on the last step', async () => {
    let msg: string | null = 'Pick at least one group.';
    const onFinish = vi.fn();
    render(<Wizard label="New policy" steps={steps(() => msg)} finishLabel="Create policy" onFinish={onFinish} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('Pick at least one group.')).toBeInTheDocument();
    expect(screen.getByText('who fields')).toBeInTheDocument();

    msg = null;
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('what fields')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('review')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => expect(onFinish).toHaveBeenCalledOnce());
  });

  it('goes back without validating and cancels', () => {
    const onCancel = vi.fn();
    render(<Wizard label="New list" steps={steps()} finishLabel="Create" onFinish={() => {}} onCancel={onCancel} initialStep={1} />);
    expect(screen.getByText('what fields')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.getByText('who fields')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it('lets a step link back to a reached step', async () => {
    function EditWho() {
      const nav = useWizardNav();
      return <button type="button" onClick={() => nav.goTo('who')}>Edit who</button>;
    }
    const s: WizardStep[] = [
      { id: 'who', label: 'Who', content: <p>who fields</p> },
      { id: 'review', label: 'Review', content: <EditWho /> },
    ];
    render(<Wizard label="New group" steps={s} finishLabel="Create" onFinish={() => {}} onCancel={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Edit who' }));
    expect(await screen.findByText('who fields')).toBeInTheDocument();
  });
});
