// Driving Astryx controls in the browser. A Selector (SelectField,
// EnumSelect) is a combobox button whose options live in the listbox
// popover it controls; SelectField puts the value on the trigger as
// data-value.
import { expect, type Locator } from '@playwright/test';

/** Opens the Selector and picks the option with this name (string = exact). */
export async function choose(trigger: Locator, option: string | RegExp): Promise<void> {
  await trigger.click();
  const id = await trigger.getAttribute('aria-controls');
  const page = trigger.page();
  const list = id ? page.locator(`[id="${id}"]`) : page.getByRole('listbox');
  await list.getByRole('option', { name: option, exact: typeof option === 'string' }).click();
  if (typeof option === 'string') await expect(trigger).toContainText(option);
}
