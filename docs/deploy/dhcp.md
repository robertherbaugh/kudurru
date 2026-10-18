# DHCPv4 server

Use kudurru to assign IPv4 addresses and publish local names for your devices.
DHCP is included in v0.1.0, disabled by default, and runs on Linux.
Keep IPv6 router and DNS advertisements on your router; DHCPv6 is not supported.

## Set up DHCP step by step

Choose [Kubernetes DHCP](../guide/dhcp/kubernetes/README.md) for the LoadBalancer,
router relay and required trusted relay setup.
Choose [Linux with SQLite](../guide/dhcp/README.md#linux-with-sqlite) for the following direct-LAN steps.
Both paths include screenshots and end with a verified client lease.

| Step | Result |
|---|---|
| [1. Prepare your network](../guide/dhcp/prepare.md) | A fixed server address, a pool and a host with the required privileges |
| [2. Describe the subnet](../guide/dhcp/create-scope.md) | An interface, server address and subnet |
| [3. Set the address pool](../guide/dhcp/address-pool.md) | Pool boundaries, a router and a DNS server |
| [4. Set lease DNS](../guide/dhcp/lease-dns.md) | A domain for client names |
| [5. Review and save](../guide/dhcp/review-scope.md) | A complete scope saved in the dashboard |
| [6. Activate and check](../guide/dhcp/activate.md) | One test client with an address and working DNS |
| [7. Reserve an address](../guide/dhcp/reservations.md) | A device that keeps the same address |

For Kubernetes, use [DHCP on Kubernetes](dhcp-kubernetes.md).
Clients reach the elected serving replica through a trusted relay and a LoadBalancer address.
Independent DHCP allocators must never share an address pool.

## Deployment and reference

| Topic | Detailed guide |
|---|---|
| <a id="kubernetes"></a>Kubernetes | [Relays, the LoadBalancer and serving replicas](dhcp-kubernetes.md) |
| <a id="start-in-the-dashboard-sqlite"></a><a id="optional-file-configuration"></a>SQLite setup | [Dashboard setup and optional YAML](dhcp-configuration.md#start-in-the-dashboard-sqlite) |
| <a id="multiple-subnets-and-relays"></a>VLANs and relays | [Multiple subnets and trusted relays](dhcp-configuration.md#multiple-subnets-and-relays) |
| <a id="conflict-detection"></a>Address conflicts | [Probes and quarantine](dhcp-configuration.md#conflict-detection) |
| <a id="ntp-search-domains-routes-and-boot-settings"></a>Network options | [NTP, search domains, routes and boot settings](dhcp-configuration.md#ntp-search-domains-routes-and-boot-settings) |
| <a id="dashboard-and-api-ownership"></a>Ownership and API | [Adoption, permissions and API routes](dhcp-management.md#dashboard-and-api-ownership) |
| <a id="repair-a-setup-that-could-not-start"></a><a id="diagnose-packet-failures-after-startup"></a>Troubleshooting | [Startup repair and packet diagnosis](dhcp-management.md#repair-a-setup-that-could-not-start) |
| <a id="automatic-lease-dns"></a>Local names | [Automatic A and PTR records](dhcp-dns.md) |
| <a id="network-and-privileges"></a>Host preparation | [Container networking and systemd capabilities](dhcp-host.md) |
| <a id="three-node-automatic-failover"></a>High availability | [Three-node quorum failover](dhcp-failover.md) |
| <a id="lease-durability-and-backups"></a>Persistent state | [Lease durability and backups](dhcp-leases.md#lease-durability-and-backups) |
| <a id="lease-import-and-export"></a>Migration | [Lease import and export](dhcp-leases.md#lease-import-and-export) |
| <a id="first-cutover-and-acceptance"></a>Cutover | [Acceptance checks and rollback](dhcp-cutover.md) |
