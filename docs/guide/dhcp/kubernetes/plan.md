# Plan DHCP and relay addresses

**Before you start**

- You need access to your router and the current DHCP configuration.
- You need a working kudurru DNS Service that clients can reach.

## Record your network values

1. Record the subnet and gateway of one client LAN or VLAN.
2. Record all static addresses, reservations and unexpired DHCP leases.
3. Choose a pool that excludes addresses still in use.
4. Choose a free LoadBalancer address that the router can reach.
5. Record the address of the kudurru DNS listener.

Use this example to distinguish the addresses:

| Purpose | Example | Where you use it |
|---|---|---|
| Client subnet | `192.0.2.0/24` | **Client subnet** in the scope |
| Client gateway | `192.0.2.1` | **Router**, and normally the relay's gateway field |
| Address pool | `192.0.2.100`–`192.0.2.199` | **First address** and **Last address** |
| DNS listener | `192.0.2.2` | **DNS servers 1** |
| DHCP LoadBalancer | `10.200.200.67` | Helm `dhcp.serverAddress` and the router's relay destination |
| Relay source | `192.0.2.1` | **Relay source address**; confirm this on your router |
| Relay gateway (`giaddr`) | `192.0.2.1` | **Relay gateway address**; identifies the client network |

The relay source and gateway can differ. A router can send from an uplink
address while it puts the client VLAN address in `giaddr`.
Neither trusted address is the DHCP LoadBalancer address.

The DHCP Service exposes UDP 67. It does not automatically expose DNS on port 53.
Use your DNS Service address for clients, even when the DHCP address is different.

CAUTION: Existing leases stay valid after you stop the old server.
Use a nonoverlapping pool, import the final leases, or wait for them to expire.
Do not rely on conflict probes to replace lease migration.

## Check the result

You have your own values for each row and a plan for existing leases.
Continue to [Enable the DHCP Service](enable.md).
