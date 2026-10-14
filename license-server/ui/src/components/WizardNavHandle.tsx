// Hands the Wizard's step navigation (ui/components/Wizard useWizardNav,
// only reachable inside a step) to the wizard component around it, so the
// finishing call can take the admin to the step a server refusal names and
// the review step's check can go back to the step that fails. Rendered in
// the review step, the step a wizard finishes from.
import { useEffect } from 'react';
import { useWizardNav } from '../ui/components/Wizard';

export type WizardNav = ReturnType<typeof useWizardNav>;

export function WizardNavHandle({ navRef }: { navRef: { current: WizardNav | null } }) {
  const nav = useWizardNav();
  useEffect(() => {
    navRef.current = nav;
  });
  return null;
}
