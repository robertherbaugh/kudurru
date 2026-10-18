# DHCP on Kubernetes

[DHCP guides](dhcp.md) · [Kubernetes walkthrough](../guide/dhcp/kubernetes/README.md)

## Kubernetes

The serving replica sits on the pod network, so clients reach it through a
DHCP relay: the router forwards each client VLAN's DHCP to a LoadBalancer
address. Leases live in the apiserver; a replica that takes over keeps every
client's address. Every scope requires at least one trusted relay.
For illustrated instructions, start with the [Kubernetes walkthrough](../guide/dhcp/kubernetes/README.md).
How it works: [DHCP on the Kubernetes store](../architecture/dhcp.md#kubernetes-store).

1. Install the `kudurru-crds` chart of this release first. It adds the
   `DHCPScope` and `DHCPLease` CRDs; kudurru waits for every kind it watches.
2. Pick a free LoadBalancer address the router can reach, and enable DHCP in
   the `kudurru` chart:

   ```yaml
   dhcp:
     enabled: true
     serverAddress: 10.200.200.67   # relays send here; option 54
     service:
       annotations:
         metallb.io/loadBalancerIPs: 10.200.200.67
   ```

   The chart adds a `<release>-dhcp` Service (UDP 67,
   `externalTrafficPolicy: Local`) that selects only the serving pod, RBAC for
   `dhcpleases` and pod labels, the low-port sysctl for UDP 67, and
   `dhcp.enabled`, `dhcp.serverIP` and `dhcp.interface` (default `eth0`) in
   the node configuration.

   RBAC cannot limit `patch` on pods to kudurru's own pods, because a
   Deployment's pod names are random. The chart therefore also installs a
   cluster-scoped ValidatingAdmissionPolicy and binding: a pod update by
   kudurru's ServiceAccount may only set or remove `kudurru.io/dhcp-active`,
   and only on this release's kudurru pods. Whoever installs the chart needs
   the right to create those (Argo CD and cluster administrators normally
   have it).
3. Point the router's DHCP relay for each client VLAN at that address, and
   allow UDP 67 from the router to it. Note the relay's source address as the
   LoadBalancer sees it and the gateway address (giaddr) it writes for the
   VLAN; both go into the scope's `trustedRelays`.
4. Create the scopes in **DNS → DHCP**, or apply them from Git:

   ```yaml
   apiVersion: kudurru.io/v1alpha1
   kind: DHCPScope
   metadata:
     name: iot
     annotations:
       kudurru.io/managed-by: gitops:argocd   # read-only in the dashboard
   spec:
     subnet: 192.168.30.0/24
     rangeStart: 192.168.30.100
     rangeEnd: 192.168.30.199
     router: 192.168.30.1
     dnsServers: [192.168.10.53]
     domainName: iot.home.arpa
     registerDNS: true
     trustedRelays:
       - sourceIP: 192.168.30.1
         gatewayIP: 192.168.30.1
     reservations:
       - {mac: "02:42:ac:11:00:02", ip: 192.168.30.10, hostname: thermostat}
   ```

   Omit the annotation for scopes you edit in the dashboard. A scope with any
   other `kudurru.io/managed-by` owner is read-only there and shows the owner.
5. Check `kubectl get dhcpscopes`: `SERVING` names the replica that answers
   and `READY` turns `True`. Stop the old DHCP service before enabling relay
   traffic to kudurru. Import final leases, use a nonoverlapping pool, or wait
   for old leases to expire. Then enable the relay and test one client.
   `kubectl get dhcpleases -n <namespace>` lists leases as clients bind.
   Follow [Switch and test](../guide/dhcp/kubernetes/check.md) for the full cutover.

Changes apply live; no restart is needed. A change to a scope's subnet,
relays or conflict detection rebuilds the DHCP listener for a moment. The
dashboard refuses an edit that a live lease contradicts. The same change from
Git applies anyway: the affected leases are not renewed and end at expiry,
and the scope's `status.conflictingLeases` counts them. Every scope is
relay-only; ICMP conflict detection (the default) uses an unprivileged ICMP
socket in the pod, and `conflictDetection: disabled` turns it off.
`dhcp.scopes`, the flat `dhcp.*` scope keys and quorum failover are rejected
with this store, and `hostNetwork.enabled` is not supported.

Back up scopes with the usual `kudurru` backup (they are `kudurru.io`
objects). Leases are runtime data and are not in backups; export them from
a scope's page (or `GET /api/v1/node/dhcp/v4/scopes/{scope}/export`) before
deleting a namespace. An import writes one `DHCPLease` per lease while the
scope waits, so the scope answers no clients until it finishes; an import
that takes more than five minutes is rolled back. Split very large imports.

## Check relay packets

If no lease appears, confirm the LoadBalancer address, Service endpoints,
scope readiness, router relay interface and firewall rules first.
`externalTrafficPolicy: Local` preserves the source at the Service;
upstream NAT can still change it. The trusted source must match what the pod receives.

Use the router's packet capture tool to inspect forwarded requests to UDP 67.
On a Linux router with `tcpdump`, replace `eth0` with the outgoing interface:

```sh
sudo tcpdump -ni eth0 -vvv 'udp dst port 67 and dst host 10.200.200.67'
```

Renew the test client's lease during the cutover. Look for a line such as
`192.0.2.1.67 > 10.200.200.67.67` and `Gateway-IP 192.0.2.1` in the decoded packet.

| Packet field | Required scope value |
|---|---|
| IPv4 source | `trustedRelays[].sourceIP` |
| `Gateway-IP` (`giaddr`) | `trustedRelays[].gatewayIP` |
| UDP source port | `sourcePort`; `0` selects port 67 |
| Link selection or server identifier override | Exact optional `linkSelection` or `serverIdentifier` |

If a request leaves the router but never reaches the serving pod, check routing,
the LoadBalancer, network policies and firewalls. A capture on the router does
not reveal NAT farther along the path; compare it with the cluster network capture.
If the pod receives requests but drops them, check the exact trusted values
and the serving replica's logs. Debug logging includes packet rejection reasons.

If a client binds but later fails renewal, allow direct client traffic to the
LoadBalancer on UDP 67. If addresses work but names fail, check the advertised
DNS address and UDP/TCP 53. The DHCP Service does not provide DNS by itself.
