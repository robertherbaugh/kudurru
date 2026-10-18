# Prepare your network for DHCP

**Before you start**

- You need the `admin` role and access to the Linux host.
- You need a running node with the SQLite store.
- You need a copy of the current DHCP options, reservations and leases.

CAUTION: Two independent DHCP servers on one subnet can assign the same address.
Keep the current service active until cutover. Stop it before you activate kudurru.

## 1. Plan the addresses

Record these values before you open the wizard. These example addresses
match the screenshots. Replace them with your own network values.

| Item | Example | What to check |
|---|---|---|
| Scope name | `lan` | A stable name for the subnet |
| Host interface | `eth0` | An existing interface on the client network |
| Server address | `192.0.2.2` | Already assigned to that interface; fixed and outside the pool |
| Client subnet | `192.0.2.0/24` | The network that contains the clients |
| First and last address | `192.0.2.100` to `192.0.2.199` | Free addresses inside the subnet |
| Router | `192.0.2.1` | The gateway for the clients |
| DNS server | `192.0.2.2` | Your node, reachable on UDP and TCP port 53 |
| Domain name | `lan.home.arpa` | A dedicated domain for lease names |

Keep static addresses outside the pool. Account for leases that the old
server already issued. Use a nonoverlapping pool if you cannot import those leases.

## 2. Prepare the host

Choose the instructions for your deployment:

| Deployment | Required preparation |
|---|---|
| Docker on Linux | [Build the DHCP image and use host networking](../../deploy/dhcp-host.md#network-and-privileges) |
| Linux package with systemd | [Add the capability drop-in](../../deploy/dhcp-host.md#network-and-privileges) |
| Kubernetes | Use the separate [relay and LoadBalancer guide](../../deploy/dhcp-kubernetes.md) |

The published Docker image lacks the file capabilities that DHCP needs.
Build from a source checkout with `make docker-dhcp`, then follow the host guide's run command.
Keep the existing data volume and secret files when you replace a container.

The dashboard cannot assign a host address or grant capabilities.
Direct clients need broadcast connectivity on the selected interface.
A published Docker UDP port alone does not provide it.

## Check the result

1. Run `ip -4 addr show` on the host.
2. Find the fixed server address on the interface you plan to select.
3. Confirm that the node uses the DHCP-capable image or the systemd drop-in.
4. Confirm that you can stop the old DHCP service during cutover.

For VLANs or routed clients, read [Multiple subnets and relays](../../deploy/dhcp-configuration.md#multiple-subnets-and-relays).
