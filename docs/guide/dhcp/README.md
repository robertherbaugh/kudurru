# Set up DHCP

Let kudurru assign IPv4 addresses, keep reservations and serve names for leased devices.
Follow this walkthrough for one Linux node with the SQLite store.

**Before you start**

- You need the `admin` role and access to the Linux host.
- You need access to your current DHCP server or router.
- You need one test client on the same network.

| Step | What you do |
|---|---|
| [1. Prepare your network](prepare.md) | Choose addresses and provide host networking and privileges |
| [2. Describe the subnet](create-scope.md) | Select an interface, server address and subnet |
| [3. Set the address pool](address-pool.md) | Set the pool, router and DNS server |
| [4. Set lease DNS](lease-dns.md) | Choose a local domain and name registration |
| [5. Review and save](review-scope.md) | Check all values and save the scope |
| [6. Activate and check](activate.md) | Stop the old service, restart the node and test a lease |
| [7. Reserve an address](reservations.md) | Keep a device at a known address |

A scope describes one subnet and its address pool. Saving a scope does not start
DHCP on a SQLite node. Activation needs a node restart.

For a Kubernetes deployment, follow [DHCP on Kubernetes](../../deploy/dhcp-kubernetes.md).
It uses trusted relays and one elected serving replica. Scope changes apply live.
For several Linux nodes, see [Automatic failover](../../deploy/dhcp-failover.md).

DHCPv6 is not supported. Keep IPv6 router and DNS advertisements on your router.
The screenshots show demo data; use your own network values.
