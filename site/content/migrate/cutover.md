---
title: DNS cutover and rollback checklist
linkTitle: Cutover checklist
description: Prepare a parallel kudurru installation, test DNS and client policies, switch IPv4 and IPv6 DNS advertisements, and recover if a check fails.
weight: 40
---

Use this checklist with the [Pi-hole](/migrate/pihole/),
[AdGuard Home](/migrate/adguard-home/) or
[Technitium](/migrate/technitium/) guide. Do not change the network's DNS
advertisements until the replacement passes the validation checks.

## Prepare

1. Back up your current DNS server using its guide. Record its addresses,
   DHCP options, IPv6 DNS advertisements, upstreams, local domains and reverse
   zones. Save a copy of the router configuration before editing it.
2. Inventory services beyond blocking: DHCP, DNSSEC, recursive resolution,
   secondary zones, dynamic updates, encrypted endpoints and plugin behavior.
   Choose a replacement for every feature kudurru v0.1.0 does not provide.
   Recursion and DNSSEC validation are available in upstream groups set to
   `mode: recursive`; DNSSEC signing is not.
3. Give kudurru a fixed address on a separate host or VM. Two servers cannot
   bind the same address and port. If you use one host, configure separate
   bind addresses or publish different test ports explicitly. The examples
   below assume separate addresses, both on port 53.
4. Install using [Docker or packages](/deploy/docker/) or
   [Helm](/deploy/helm/). Keep network DNS pointed at the old server.
   Allow UDP **and** TCP port 53 from the intended client networks. Restrict
   the management listener to administrators.
5. Sign in to `https://<new-server>:8080/`, change the initial administrator
   password and install an appropriate [license key](/project/licensing/)
   before creating policies beyond `default`.

The sample addresses `192.0.2.53` (old server), `192.0.2.54` (new server)
and `192.0.2.10` (test client) are documentation placeholders. Replace them
with reachable addresses from your network. Replace `home.arpa` if your local
zone is different. Preserve existing names first; rename them separately later.

### Connect the CLI

Install the `kudurru` binary matching the server version. The CLI uses the
management API; it does not need to run on the DNS host.

```sh
kudurru login --server https://192.0.2.54:8080 --username admin \
  --name migration --default-namespace default --ca-file kudurru-ca.pem
kudurru config get-contexts
kudurru get policies -A -o yaml
kudurru get clientgroups -A -o yaml
```

Use the certificate that signs the management listener as `kudurru-ca.pem`.
For the default self-signed listener, trust its certificate, or use
`--insecure-skip-tls-verify` instead of `--ca-file` for your isolated test.
`--name migration` selects a context for subsequent commands. Check its
server address before applying objects.

### Use the right namespace and owner

The examples in these guides use `namespace: default`, matching a fresh
standalone installation. Helm normally seeds objects in the release
namespace; older installations may retain `default`. Inspect the output
above and change **every** namespaced example and the CLI context to match
where your `Policy/default` lives. Use `-n <namespace>` to override the
context for a command. Lists and groups referenced by a policy must be in
that policy's namespace. `DNSZone` is cluster-scoped and has no namespace.

Choose one way to own an object:

- **Dashboard or `kudurru apply -f`:** API-managed objects remain editable in
  the dashboard. This is the path used by the examples below.
- **`objects.d/`:** files are authoritative and their objects are read-only
  in the dashboard. Keep list references in those files too.
- **Kubernetes:** use `kubectl apply` for CRDs and edit their manifests.
  `kudurru apply` refuses Kubernetes-owned objects with `managed_by`.

Do not apply the same object through several owners. See the
[configuration layout](/deploy/docker/#configuration).

## Recreate the configuration

Follow your product's guide. Before testing, verify these shared details:

- A new installation includes the `starter` block list and a default policy.
  Replace the default policy's list references deliberately; leaving starter
  attached can explain a block that did not exist on the old server.
- Creating a `BlockList` does not activate it. Reference it from a policy's
  `blockListRefs` or `allowListRefs` as appropriate.
- An exact-IP group match takes precedence over a CIDR match; a longer
  prefix takes precedence over a shorter one. Default membership is a
  fallback. A specific group's policy does not inherit the default policy's
  lists. Include every baseline list that group needs.
- Use IP/CIDR matching in v0.1.0, including IPv6 addresses or prefixes where
  applicable. MAC, hostname and encrypted-transport client-ID fields do not
  have production identity sources in this release.
- Preserve source addresses. A router that proxies DNS makes all devices
  look like the router; NAT or a Kubernetes Service can do the same. Send
  clients directly to kudurru, or use a deployment that preserves source
  addresses. Check the real query log before trusting a client-group rule.
- Configure upstreams independently of the old server unless retaining it
  as an upstream is intentional. Avoid forwarding loops. A hostname-based
  upstream needs bootstrap resolution that does not depend on that upstream.
- Inspect each list's status. `status.invalid` and `status.unsupported` count
  different kinds of skipped rules. Resolve unexpected skipped rules and
  failed downloads before proceeding.

## Validate

First check object readiness and the policy explanation:

```sh
kudurru get blocklists -o yaml
kudurru get policies -o yaml
kudurru query tracker.example A --client 192.0.2.10
kudurru query example.com AAAA --client 192.0.2.10
kudurru query nas.home.arpa A --client 192.0.2.10
```

`tracker.example` is a deterministic rule included in the product examples.
`kudurru query --client` evaluates the server's pipeline through the API as
that client; it does **not** prove that a real device reaches port 53 or
arrives with that source address. Run the following from the actual test
client as well:

```sh
dig @192.0.2.53 example.com A +noall +comments +answer
dig @192.0.2.54 example.com A +noall +comments +answer
dig @192.0.2.54 example.com AAAA +noall +comments +answer
dig @192.0.2.54 example.com A +tcp +noall +comments +answer
dig @192.0.2.54 tracker.example A +noall +comments +answer
dig @192.0.2.54 nas.home.arpa A +noall +comments +answer
```

Repeat each relevant query against both servers. Public answers and TTLs can
differ because of caching and CDN routing; compare successful resolution,
response codes and intended filtering rather than identical output.

| Check | Pass condition |
|---|---|
| Public A and AAAA, UDP and TCP | Expected records, no unexpected timeout, REFUSED or SERVFAIL |
| Blocked name and a subdomain | Expected block response for both A and AAAA; the explanation names the intended list/rule |
| Explicit allow exception | Resolves even when an attached block list also matches |
| Local A, AAAA, CNAME and other records in use | Intended targets; no missing record types |
| Reverse lookup | `dig @192.0.2.54 -x <device-address>` reaches the correct PTR data or private reverse resolver |
| Default and custom client groups | One real device per group shows the expected source IP, group, lists and upstream |
| Schedules and safe search, if used | Expected behavior inside and outside each time window and for the supported engines |
| Encrypted DNS, if used | Actual client connects using the configured hostname, certificate, port and protocol |
| IPv6 client DNS | Real queries reach kudurru and match the intended IPv6 group |
| Recursive group, if used | `dig @192.0.2.54 isc.org A +dnssec` has the `ad` flag; a name in your router's local domain resolves through its conditional rule; the group's resolver is healthy on **Upstreams** (outbound UDP/TCP 53 is open) |

Set one test device's DNS to kudurru while the rest of the network uses the
old server. Test normal browsing, work VPNs, streaming, local applications
and any service that needed an allow exception. Check **Query log** for its
real address and investigate each unexpected result. Configure browser or
OS secure DNS deliberately; an external DoH provider bypasses these servers.

## Switch over

1. Save the tested kudurru objects with
   `kudurru backup --out before-cutover.yaml`. Save settings, certificates and
   credentials separately; an object backup is not a full data-volume backup.
2. If the old DNS server provides DHCP, complete the DHCP handoff described
   in its migration guide. kudurru v0.1.0 includes opt-in DHCPv4 on
   Linux/SQLite; use the [DHCPv4 deployment guide](/deploy/dhcp/) or keep
   your existing DHCP server. Preserve address reservations, active leases,
   lease ranges, gateway and search-domain options.
3. Change DHCPv4's DNS option to kudurru. Also update DHCPv6 and router
   advertisements (RDNSS), including mesh routers or access points that
   advertise their own DNS settings. Review static devices and VPN profiles.
4. Renew the test client's lease or reconnect it, remove its temporary manual
   DNS override, and confirm the assigned resolver addresses. Test a query
   without `@server` and confirm it appears in kudurru's query log.
5. Renew other clients gradually and observe both servers. Keep the old
   server running at its original address for rollback. Use an observation
   period of at least the longest DHCP lease and relevant DNS cache lifetime,
   plus your normal usage cycle.

Do not list a public resolver or the old server as a supposed standby if
you require kudurru's policy on every query. Clients can use any advertised
resolver. For redundancy, advertise servers with the same intended policy
and test each independently.

## Roll back

Roll back if clients lose required DNS, local applications fail, or policies
differ in a way you cannot resolve during the change window.

1. Restore the old DNS addresses in DHCPv4, DHCPv6, RDNSS, static devices and
   VPN profiles. Revert any encrypted-DNS endpoint changes too.
2. If DHCP moved, use the saved DHCP plan to restore service. Disable the
   replacement DHCP server before re-enabling the original on the same LAN.
3. Renew affected leases or reconnect devices. Clear client DNS caches when
   necessary. Restoring a DHCP option alone does not update existing clients
   immediately.
4. Confirm an ordinary query reaches the old server and the failed
   application works again. Leave kudurru on its separate address to
   investigate logs and configuration.

After a successful migration, take a new backup, record the final DNS
advertisements and retire the old service only after its query log stays
quiet through the observation period. Use the [backup instructions](/deploy/docker/#persistence-and-backups)
for ongoing recovery.
