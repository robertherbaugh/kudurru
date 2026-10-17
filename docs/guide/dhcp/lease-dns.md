# Set DNS for DHCP clients

**Before you start**

- You need the `admin` role and the open **New DHCP scope** wizard.
- You need to complete the [Addresses step](address-pool.md).

## Set the local domain

1. Click **Next** to open **DNS**.
2. Enter `lan.home.arpa` in **Domain name**, or use your planned domain.
3. Turn on **Register lease names in DNS** if you want A and PTR records.
4. Keep **Lease record TTL** at `30s` for this example.

![The DNS step with domain lan.home.arpa, lease name registration on and a record TTL of 30 seconds.](../images/dhcp-wizard-dns.png)

*Figure 1. Use a dedicated domain for lease names.*

A more specific configured zone takes precedence over automatic lease names.
See [Lease DNS](../../deploy/dhcp-dns.md#automatic-lease-dns) if a name does not resolve.

## Check the result

The domain matches your plan. If registration is on, active clients can get
names such as `laptop.lan.home.arpa`. A reservation alone does not publish a name.
Keep the wizard open and continue to [Review and save](review-scope.md).
