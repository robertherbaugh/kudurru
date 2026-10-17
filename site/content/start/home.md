---
title: Home network in one container
linkTitle: Home network
audience: Home user
description: One `docker run` or compose file, the dashboard on port 8080, blocking with per-device rules, and local names for the things on your network.
weight: 10
---

One container is the whole server: DNS on port 53, the dashboard and the
API on 8080, and everything it stores (objects, users, the query log, list
caches) in one volume. Nothing else is needed.

## 1. Start it

Follow [One container](/deploy/docker/) for `docker run`, the compose file
in `deploy/compose/single` (read-only root filesystem, no capabilities,
Docker secrets) or the deb, rpm and apk packages with systemd. Without a
configuration file kudurru forwards to 1.1.1.1 and 9.9.9.9, blocks with the
embedded starter list and keeps a query log.

If the host runs systemd-resolved, its stub listener holds port 53: set
`DNSStubListener=no` in `/etc/systemd/resolved.conf` first.

## 2. Sign in

Open `https://<host>:8080/`. The API listener generates a self-signed
certificate at the first start (`listeners.api.tls.auto`); use your own with
[`listeners.api.tls.certFile`](/reference/configuration/#listeners.api.tls.certFile),
or turn TLS off behind a reverse proxy. Sign in as `admin` with the password
from `KUDURRU_ADMIN_PASSWORD_FILE` (or the one logged once at the first
start); the dashboard asks for a new one. A lost password is reset on the
host with `kudurru admin reset-password admin`.

## 3. Point the network at it

Give the host a fixed address and make it the DNS server your router hands
out with DHCP (often "DNS server" or "primary DNS" in the router's LAN
settings). kudurru v0.1.0 also includes an optional DHCPv4 server if you want
it to assign addresses. Check from any device:

```sh
dig @<host> example.com
```

To use kudurru for DHCPv4, follow the [DHCPv4 setup guide](/deploy/dhcp/).
It supports Linux/SQLite standalone or three-node quorum operation,
[Kubernetes](/deploy/dhcp/#kubernetes) with one elected replica behind a
relay, and dashboard setup. For Docker, build a local DHCP-capable image from source
with `make docker-dhcp`; the published images lack the required file
capabilities. Packaged systemd installs need a capability drop-in. Follow
[Network and privileges](/deploy/dhcp/#network-and-privileges) before setup.

DHCPv4 is disabled by default; stop the existing DHCPv4
service before activating it. Keep IPv6 router and DNS advertisements on
your router; DHCPv6 is not supported.

## 4. Blocking

Lists are `BlockList` objects: URLs or inline entries in hosts, domains,
adblock or regex format, refreshed on an interval
([BlockList reference](/reference/crds/blocklist/)). Add them in the
dashboard, or as YAML in the configuration's `objects.d/` directory:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: BlockList
metadata: {name: default-ads, namespace: dns}
spec:
  mode: block
  sources:
    - url: https://big.oisd.nl/
  refreshInterval: 24h
```

Every answer in the query log says why it was given (which list and rule
blocked it, or which upstream answered), with one-click allow and deny.
From the command line: `kudurru lists allow example.com`,
`kudurru lists search example.com`, and `kudurru blocking disable 5m` for a
pause that ends by itself.

## 5. Rules per device

A `ClientGroup` matches devices by IP address or CIDR in v0.1.0. Hostname,
MAC and DoT/DoH client-ID fields are reserved for future inventory and
transport support. A `Policy` gives a group its lists, a schedule, a block response
and its own upstreams. Devices in no group get `Policy/default`. A policy
other than `Policy/default` needs a [license key](/project/licensing/):
without one, kudurru creates the group below but refuses the policy with
`license_required`. A free personal key covers a home network.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: kids, namespace: dns}
spec:
  clients:
    - cidr: 10.0.20.0/24
      name: Kids VLAN
    - ip: 192.168.1.50
      name: Game console
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata: {name: kids, namespace: dns}
spec:
  clientGroupRefs: [kids]
  blockListRefs: [default-ads]
  schedule: {weekdays: [mon, tue, wed, thu, fri], start: "08:00", end: "20:00", tz: Europe/Berlin}
```

See the [ClientGroup](/reference/crds/clientgroup/) and
[Policy](/reference/crds/policy/) references.

## 6. Names for your own devices

Local names are a primary zone of your own (a `DNSZone`) with records in it.
Create the zone in the dashboard or from YAML ([DNSZone](/reference/crds/dnszone/),
example at the end of that page), then add records:

```sh
kudurru login --server https://<host>:8080 --username admin --insecure-skip-tls-verify
kudurru records add home-example-com nas.home.example.com A 192.168.1.10
```

`--insecure-skip-tls-verify` accepts the self-signed certificate; pass
`--ca-file` with the certificate instead to verify it. After the login,
`kudurru query nas.home.example.com` resolves through the server's own
pipeline and prints why it answered the way it did. Zones you already
keep in BIND format import with `kudurru import bind <zone> -f file.zone`.

## 7. Resolve names yourself (optional)

Out of the box kudurru forwards to 1.1.1.1 and 9.9.9.9, so those two
providers see every name your network looks up. Switch the `default`
upstream group to recursive mode and kudurru asks the root servers and each
zone's own name servers instead, and validates DNSSEC on the way; there is
no need for a separate Unbound. In the dashboard open **DNS** >
**Upstreams** > `default`, select **Recursive** as the **Mode** and save.
In YAML, with the router kept for its local names:

```yaml
upstreams:
  groups:
    default: { mode: recursive, timeout: 4s }
    router:  { servers: ["192.168.1.1:53"] }
  conditional: { "[/lan/1.168.192.in-addr.arpa/]": router }
```

- **Privacy.** No single provider sees every name. Each authoritative
  server still sees the names in its zone and your public address; QNAME
  minimization keeps the root and TLD servers to the part they need
  (`com`, then `example.com`). The queries to authoritative servers are
  plain DNS on port 53, not encrypted, so your ISP can read them. No client
  subnet is ever sent.
- **Network.** The host needs outbound UDP **and** TCP port 53 to any
  address. Where a firewall or the ISP blocks or intercepts port 53, the
  group's resolver goes into backoff and names fail; keep an encrypted
  forwarder there instead.
- **Local names.** The root servers do not know your router's names
  (`nas.lan`, `fritz.box`): send its domain and your reverse zone to a
  forwarding group, as above. Private reverse zones and special-use names
  (`home.arpa`, `local`, `internal`, ...) never leave the network: kudurru
  answers them with NXDOMAIN unless a rule sends them to the router.
- **DNSSEC.** An answer that fails validation becomes SERVFAIL with an
  extended DNS error (6 to 12) and the query-log status `dnssec_bogus`;
  `dig +cd` still shows the data. Unsigned zones resolve as before.
- **Speed.** A cold lookup takes a few round trips (about 50 to 500 ms);
  the cache answers repeats.

The options are the `upstreams.recursion` settings, for example
[`upstreams.recursion.dnssec`](/reference/configuration/#upstreams.recursion.dnssec).
The [administrator guide](/guide/admin/07-zones-and-upstreams/#resolve-names-from-the-root-servers)
walks through it, and the [architecture tour](/architecture/upstreams-and-cache/#recursive-groups)
explains how it works.

## 8. Keep it running

- **Settings** (upstreams, cache, blocking, query-log privacy and retention,
  rate limiting) change in the dashboard or with `kudurru settings set`; most
  apply at once. The [settings reference](/reference/configuration/) lists every key.
- **Backups**: `kudurru backup --out kudurru.yaml` saves every object as YAML
  that `kudurru restore -f` applies again; a copy of the data volume taken
  with the container stopped also keeps users, tokens and the query log.
- **Encrypted DNS** (DoT, DoH, DoQ): add the listeners and a certificate, or
  put dnsdist in front with `deploy/compose/single-edge` (the edge needs a
  [license key](/project/licensing/)); see
  [encrypted transports](/deploy/docker/#encrypted-transports).
- **Coming from Pi-hole, AdGuard Home or Technitium?** The [migration guides](/migrate/)
  map their settings onto kudurru objects.
