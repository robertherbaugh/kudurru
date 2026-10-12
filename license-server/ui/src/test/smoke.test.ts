// The UI test runner works and sees the shared catalog through the @shared
// alias (vite.config.ts, ui/tsconfig.json). Stream C's page tests replace
// this file.
import { planLabel } from '@shared/catalog';

test('the shared catalog resolves in the UI tests', () => {
  expect(planLabel('personal')).toBe('Personal (free, non-commercial)');
  expect(document.createElement('p')).toBeInstanceOf(HTMLElement);
});
