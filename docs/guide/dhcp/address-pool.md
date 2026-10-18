# Set the DHCP address pool

**Before you start**

- You need the `admin` role and the open **New DHCP scope** wizard.
- You need to complete the [Subnet step](create-scope.md).

## Set the address pool

1. Click **Next** to open **Addresses**.
2. Enter the pool boundaries in **First address** and **Last address**.
3. Enter the gateway address in **Router**.
4. Click **Add DNS server**.
5. Enter the kudurru address in **DNS servers 1**.
6. Keep **Lease duration** at `12h` for this example.

![The Addresses step with the pool 192.0.2.100 to 192.0.2.199, router 192.0.2.1 and DNS server 192.0.2.2.](../images/dhcp-wizard-addresses.png)

*Figure 1. These options go to clients when they acquire or renew a lease.*

Keep the additional network and boot options at their defaults unless your network needs them.

## Check the result

The first and last addresses fit the subnet. The pool excludes static addresses
and addresses that the old server still leases to other clients.
The DNS server is your kudurru node.
Keep the wizard open and continue to [Set lease DNS](lease-dns.md).
