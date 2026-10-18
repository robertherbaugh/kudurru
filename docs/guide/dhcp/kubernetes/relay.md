# Configure the router DHCP relay

**Before you start**

- You need access to the router and its DHCP relay documentation.
- You need the [DHCP LoadBalancer address](enable.md).
- You need the client VLAN and its gateway address.

## Prepare the relay

CAUTION: Keep the relay disabled until [Switch and test](check.md).
Some routers cannot run a DHCP server and relay on the same interface.
Make that change during the cutover if your router cannot stage it.

1. Open the router's DHCP relay or IP helper settings.
2. Select the client LAN or VLAN interface.
3. Set the destination server to the DHCP LoadBalancer address, `10.200.200.67` in this example.
4. Record the source IPv4 address that the router uses for forwarded packets.
5. Record the gateway address (`giaddr`) that the relay puts in packets for this VLAN.
6. Save the settings without forwarding client traffic yet.

The exact menus depend on the router. Use its relay settings, not its DNS
forwarder settings. Do not select a pod IP or a Kubernetes ClusterIP as the destination.

## Match the trusted values

| Router behavior | kudurru field | Example |
|---|---|---|
| IPv4 source of the forwarded packet | **Relay source address** | `192.0.2.1` |
| Client-facing gateway in `giaddr` | **Relay gateway address** | `192.0.2.1` |
| UDP source port | **Relay source port** | `0` uses the normal port `67` |
| Option 82 link selection, if used | **Relay link selection** | Exact configured address |
| Server identifier override, if used | **Relay server identifier** | Exact configured address |

For a basic relay, leave the last 2 fields empty.
If the router sends from `10.200.200.1`, use that address as the source.
Keep `192.0.2.1` as the gateway for this example VLAN.
Do not assume that both fields equal the router's management address.

## Prepare the network path

1. Allow relay traffic to the DHCP LoadBalancer on UDP 67.
2. Allow replies from the serving pod through the Service to the relay.
3. Provide routes between the cluster and the client subnet.
4. Allow clients to reach the DHCP LoadBalancer on UDP 67 for direct lease renewal.
5. Allow clients to reach the DNS listener on UDP and TCP 53.

Avoid source NAT between the relay and the serving pod. The trusted source must
match the address the pod receives. Check firewalls and Kubernetes network policies.
On the client LAN, allow DHCP between the router and clients on UDP 67 and 68.
Replies go to the relay gateway (`giaddr`) on UDP 67 by default.
The cluster needs a route to that address even when the relay source is different.

## Check the result

You have a relay destination, a source address and a gateway address for this VLAN.
If the source is unclear, use the [packet check](../../../deploy/dhcp-kubernetes.md#check-relay-packets) during cutover.
Continue to [Describe the subnet](create-scope.md).
