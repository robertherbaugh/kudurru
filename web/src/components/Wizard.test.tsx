import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { newRedirect } from '../lib/router';
import { renderWithApp } from '../test/utils';
import { Wizard, WizardDialog, useWizardNav, type WizardStep } from './Wizard';

function steps(validate?: () => string | null): WizardStep[] {
  return [
    { id: 'who', label: 'Who', content: <p>who fields</p>, validate },
    { id: 'what', label: 'What', content: <p>what fields</p> },
    { id: 'review', label: 'Review', content: <p>review</p> },
  ];
}

/** A wizard in its dialog, as the creation flows render it. */
function inDialog(wizard: React.ReactNode, onClose: () => void = () => {}) {
  return renderWithApp(
    <WizardDialog isOpen onClose={onClose} title="New thing" what="thing">
      {wizard}
    </WizardDialog>,
  );
}

describe('Wizard', () => {
  it('blocks Next with the step message and finishes on the last step', async () => {
    let msg: string | null = 'Pick at least one group.';
    const onFinish = vi.fn();
    inDialog(<Wizard label="New policy" steps={steps(() => msg)} finishLabel="Create policy" onFinish={onFinish} />);
    const dialog = await screen.findByRole('dialog', { name: 'New thing' });
    expect(within(dialog).getByRole('list', { name: 'New policy' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('Pick at least one group.')).toBeInTheDocument();
    expect(screen.getByText('who fields')).toBeInTheDocument();

    msg = null;
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('what fields')).toBeInTheDocument();
    // Focus moves to the new step's heading.
    expect(screen.getByRole('heading', { level: 3, name: 'What' })).toHaveFocus();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('review')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => expect(onFinish).toHaveBeenCalledOnce());
  });

  it('goes back without validating and cancels a pristine wizard without asking', async () => {
    const onClose = vi.fn();
    inDialog(<Wizard label="New list" steps={steps()} finishLabel="Create" onFinish={() => {}} initialStep={1} />, onClose);
    expect(await screen.findByText('what fields')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    expect(screen.getByText('who fields')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalledOnce();
    expect(screen.queryByRole('alertdialog')).toBeNull();
  });

  it('asks before discarding what was entered', async () => {
    const onClose = vi.fn();
    function Typed() {
      const [v, setV] = useState('');
      const s: WizardStep[] = [{ id: 'who', label: 'Who', content: <input aria-label="Name" value={v} onChange={(e) => setV(e.target.value)} /> }];
      return <Wizard label="New thing" steps={s} finishLabel="Create" onFinish={() => {}} />;
    }
    inDialog(<Typed />, onClose);
    fireEvent.change(await screen.findByRole('textbox', { name: 'Name' }), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: /^close/i }));
    const confirm = await screen.findByRole('alertdialog', { name: 'Discard the new thing?' });
    fireEvent.click(within(confirm).getByRole('button', { name: 'Keep editing' }));
    expect(onClose).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Discard' }));
    expect(onClose).toHaveBeenCalledOnce();
  });

  it('treats the caller\'s isDirty as entered', async () => {
    const onClose = vi.fn();
    inDialog(<Wizard label="New thing" steps={steps()} finishLabel="Create" onFinish={() => {}} isDirty />, onClose);
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(await screen.findByRole('alertdialog', { name: 'Discard the new thing?' })).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('lets a step link back to a reached step', async () => {
    function EditWho() {
      const nav = useWizardNav();
      return (
        <button type="button" onClick={() => nav.goTo('who')}>
          Edit who
        </button>
      );
    }
    const s: WizardStep[] = [
      { id: 'who', label: 'Who', content: <p>who fields</p> },
      { id: 'review', label: 'Review', content: <EditWho /> },
    ];
    inDialog(<Wizard label="New group" steps={s} finishLabel="Create" onFinish={() => {}} />);
    fireEvent.click(await screen.findByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Edit who' }));
    expect(await screen.findByText('who fields')).toBeInTheDocument();
  });
});

describe('newRedirect', () => {
  it('replaces /x/new with the list page and ?new=1, keeping the other parameters', async () => {
    const Page = newRedirect('/policies');
    renderWithApp(<Page />, { route: '/policies/new?namespace=lab' });
    await waitFor(() => expect(window.location.pathname + window.location.search).toBe('/policies?namespace=lab&new=1'));
  });
});
