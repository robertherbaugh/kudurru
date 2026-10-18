# Set up DHCP on Kubernetes

**Before you start**

- You need a working kudurru Helm deployment with `mode: kubernetes`.
- You need the `admin` role, cluster administration access and access to your router.
- Your router needs a DHCP relay feature, sometimes called an IP helper.
- You need one test client and a planned maintenance window.

Kubernetes requires a trusted relay for every scope. The router receives client
broadcasts and forwards them to kudurru's DHCP LoadBalancer address.
Entering a trusted relay in kudurru does not configure the router.

| Step | Result |
|---|---|
| [1. Plan the addresses](plan.md) | A client subnet, pool, DNS address and relay addresses |
| [2. Enable the DHCP Service](enable.md) | A reachable LoadBalancer on UDP 67 |
| [3. Configure the router relay](relay.md) | A relay destination and exact trusted values |
| [4. Describe the subnet](create-scope.md) | A scope with a required trusted relay |
| [5. Set the address pool](address-pool.md) | Client addresses, gateway and DNS servers |
| [6. Set lease DNS](lease-dns.md) | A local domain for leased devices |
| [7. Review and save](review.md) | A scope that applies live |
| [8. Switch and test](check.md) | A client lease, successful renewal and working DNS |

Scope changes apply live. Helm controls whether DHCP is enabled.
Do not follow the SQLite restart or host interface instructions for this deployment.
The screenshots show demo data. Replace the example addresses with your network values.

## Check the result

After step 8, the scope has a serving replica and the test client uses kudurru.
Use the [Kubernetes reference](../../../deploy/dhcp-kubernetes.md) for Git-managed scopes and storage details.
