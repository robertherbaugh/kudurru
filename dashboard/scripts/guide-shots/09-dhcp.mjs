// DHCP walkthrough: saved setup and the running lease/reservation editors.
import { choose, fresh, getPage, go, group, settle, shot } from './lib.mjs';

export default async function () {
  await group('dhcp', async () => {
    await fresh();
    const page = getPage();
    // A new SQLite node: no saved scopes and no running allocator.
    await page.evaluate(async () => {
      const { mockDHCP, mockDHCPSetup } = await import('/src/mocks/dhcp.ts');
      mockDHCP().enabled = false;
      mockDHCP().setup = { ...mockDHCPSetup(), scopes: [], scopeRevisions: {} };
    });
    await go('/dhcp');
    await page.getByRole('button', { name: 'New scope', exact: true }).click();
    await page.getByRole('textbox', { name: 'Scope name', exact: true }).fill('lan');
    await choose(page.getByRole('combobox', { name: 'Host interface', exact: true }), /eth0/);
    await page.getByRole('textbox', { name: 'Server address', exact: true }).fill('192.0.2.2');
    await page.getByRole('textbox', { name: 'Client subnet', exact: true }).fill('192.0.2.0/24');
    await shot('dhcp-wizard-subnet');
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await page.getByRole('textbox', { name: 'First address', exact: true }).fill('192.0.2.100');
    await page.getByRole('textbox', { name: 'Last address', exact: true }).fill('192.0.2.199');
    await page.getByRole('textbox', { name: 'Router', exact: true }).fill('192.0.2.1');
    await page.getByRole('button', { name: 'Add DNS server', exact: true }).click();
    await page.getByRole('textbox', { name: 'DNS servers 1', exact: true }).fill('192.0.2.2');
    await shot('dhcp-wizard-addresses');
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await page.getByRole('textbox', { name: 'Domain name', exact: true }).fill('lan.home.arpa');
    const register = page.getByRole('switch', { name: 'Register lease names in DNS', exact: true });
    if (await register.getAttribute('aria-checked') !== 'true') await register.click();
    await shot('dhcp-wizard-dns');
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await shot('dhcp-wizard-review');
    await page.getByRole('button', { name: 'Save scope', exact: true }).click();
    await page.getByRole('heading', { name: 'Setup for next restart', exact: true }).waitFor();
    await go('/dhcp');
    await page.getByRole('switch', { name: 'Enable DHCP after restart', exact: true }).click();
    await page.getByText('Restart required', { exact: true }).waitFor();
    await settle(500);
    await shot('dhcp-restart-required', { clip: { x: 260, y: 54, width: 1020, height: 600 } });

  });
  await group('dhcp-running', async () => {
    await fresh();
    const page = getPage();
    await page.evaluate(async () => {
      const { mockDHCP } = await import('/src/mocks/dhcp.ts');
      mockDHCP().config.scope = 'lan';
    });
    await go('/dhcp/lan');
    const leases = page.getByRole('table', { name: 'DHCP leases' });
    await leases.waitFor();
    // Capture the full section, including filters and the table, at readable size.
    const section = page.getByRole('heading', { name: 'Leases', exact: true }).locator('..').locator('..');
    await section.scrollIntoViewIfNeeded();
    await shot('dhcp-leases', { locator: section });
    await page.getByRole('button', { name: 'Add reservation', exact: true }).click();
    await page.getByRole('textbox', { name: 'MAC address', exact: true }).fill('02:00:00:00:00:10');
    await page.getByRole('textbox', { name: 'Reserved address', exact: true }).fill('192.0.2.10');
    await page.getByRole('textbox', { name: 'Hostname', exact: true }).fill('printer');
    await shot('dhcp-reservation');
  });
}
