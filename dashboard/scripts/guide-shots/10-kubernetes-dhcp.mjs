// Kubernetes DHCP: the required trusted relay and a live scope.
import { fresh, getPage, go, group, shot } from './lib.mjs';

export default async function () {
  await group('dhcp-kubernetes', async () => {
    await fresh('admin', { height: 1400 });
    const page = getPage();
    await page.evaluate(async () => {
      const { mockKubeDHCP } = await import('/src/mocks/dhcp.ts');
      mockKubeDHCP({ scopes: [], scopeRevisions: {} });
    });
    await go('/dhcp');
    await page.getByRole('button', { name: 'New scope', exact: true }).click();
    await page.getByRole('textbox', { name: 'Scope name', exact: true }).fill('lan');
    await page.getByRole('textbox', { name: 'Client subnet', exact: true }).fill('192.0.2.0/24');
    await page.getByRole('button', { name: 'Add', exact: true }).click();
    await page.getByRole('textbox', { name: 'Relay source address', exact: true }).fill('192.0.2.1');
    await page.getByRole('textbox', { name: 'Relay gateway address', exact: true }).fill('192.0.2.1');
    await page.getByRole('spinbutton', { name: 'Relay source port', exact: true }).fill('0');
    await shot('dhcp-kubernetes-subnet');
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    for (const [name, value] of [['First address', '192.0.2.100'], ['Last address', '192.0.2.199'], ['Router', '192.0.2.1']]) {
      await page.getByRole('textbox', { name, exact: true }).fill(value);
    }
    await page.getByRole('button', { name: 'Add DNS server', exact: true }).click();
    await page.getByRole('textbox', { name: 'DNS servers 1', exact: true }).fill('192.0.2.2');
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await page.getByRole('textbox', { name: 'Domain name', exact: true }).fill('lan.home.arpa');
    const register = page.getByRole('switch', { name: 'Register lease names in DNS', exact: true });
    if (await register.getAttribute('aria-checked') !== 'true') await register.click();
    await page.getByRole('button', { name: 'Next', exact: true }).click();
    await shot('dhcp-kubernetes-review');
  });
  await group('dhcp-kubernetes-running', async () => {
    await fresh();
    const page = getPage();
    await page.evaluate(async () => {
      const { mockKubeDHCP, mockDHCP } = await import('/src/mocks/dhcp.ts');
      mockDHCP().config.scope = 'lan';
      mockKubeDHCP();
    });
    await go('/dhcp');
    await shot('dhcp-kubernetes-overview');
    await go('/leases');
    await page.getByRole('table', { name: 'DHCP leases' }).waitFor();
    await shot('dhcp-kubernetes-leases');
  });
}
