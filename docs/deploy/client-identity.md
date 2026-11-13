# Client identity and transport IDs

Client groups can match exact IPs, MAC addresses, CIDRs, hostnames, hostname
globs or client IDs. The first matching tier selects all matching groups:
exact IP → MAC → longest nonzero CIDR → hostname → name glob → client ID →
catch-all `/0` → default. Inventory supplies MACs and hostnames from memory.
Expired inventory stops identifying a device, and reassigned IPs never inherit
an older device's policy. SQLite can populate inventory through the background
[UniFi integration](integrations.md). The built-in DHCP server fills the MAC of
a leased address on both store drivers (node-local on a SQLite cluster; see
below). Private group contributions retain
their original source and expiry, while manual public matchers keep their
configured behavior.

## Where each identity comes from

A matcher matches only when kudurru has that identity for the query:

| Matcher | Source | What it needs |
|---|---|---|
| `ip`, `cidr` | The source address of the query, or the address in an authenticated PROXYv2 header from a trusted edge | Nothing more |
| `clientID` | The server name of a DoT or DoQ connection, or the path of a DoH or DoH3 request | The listener settings on this page |
| `mac` | An inventory integration that reports the device with its addresses, or a live lease of the built-in DHCP server on this node | A running integration ([integrations.md](integrations.md) says on which deployments integrations run), or the DHCP server leasing the address |
| `hostname`, `nameGlob` | An inventory integration and the lease names of the built-in DHCP server ([dhcp-dns.md](dhcp-dns.md)); when neither has a name for the address, the optional reverse-PTR lookup ([`clientNames.reversePTR`](../../docs-site/content/reference/configuration.md#clientNames.reversePTR.enabled)) | One of these sources for the device |

Inventory, DHCP and reverse-PTR data are looked up by the source address of
the query. They cannot tell apart devices behind a router that replaces that
address. A client ID can, because it travels with the encrypted connection,
but only when no `ip`, `mac`, `cidr`, `hostname` or `nameGlob` matcher of any
client group matches the address the query arrives from: the first matching
tier wins (`IdentifyAt` in `internal/policy/index.go`), and the client ID
tier comes after those. A catch-all `0.0.0.0/0` or `::/0` CIDR is the
exception: it comes after the client ID, so a group for every address does
not hide a client ID.
The built-in DHCP server supplies lease names to the hostname matchers and the
lease MAC to `mac` matchers. A MAC reported by an inventory integration for the
same address wins over the lease MAC.

Where a lease MAC applies depends on the store driver. On Kubernetes every
replica watches the cluster's leases, so a lease MAC selects `mac` groups for
queries answered by any replica. On a SQLite cluster the lease MAC is
node-local: only queries answered by the node whose DHCP server holds the
lease get it. The same client querying another node has no MAC there, falls
through to the CIDR, hostname, client ID or default tier, and can get a
different policy, including not being blocked by a `mac` group's block list.
For cluster-wide effect on SQLite, use an inventory integration or `ip` and
`cidr` matchers.

The block page and unblock requests identify the client by its address only
(`internal/blockpage/policyfinder.go`): they see no inventory or lease MAC,
hostname or client ID. For a client whose group a MAC or name selects, the
policy shown on the block page and the groups recorded on an unblock request
can differ from those that the DNS answer applied.

## Client inventory

`GET /api/v1/inventory` and `kudurru inventory` list every device the
inventory knows, whether or not it has sent a DNS query: the devices reported
by inventory integrations and the bound DHCP leases of the answering node.
`GET /api/v1/clients` is a different view built from the query log, so it
lists only clients that queried. Reading the inventory needs `inventory.read`
(roles auditor, operator and admin; the viewer role does not include it,
because the inventory holds MAC addresses and device names, as with
`dhcpleases.read`).

Each device shows its source (`integration-<digest>` for an integration,
`dhcp/<scope>` for a lease), MACs, names, tags, observation and expiry times,
`stale` once its observation or lease has expired, and the state of each
address:

| State | Meaning |
|---|---|
| `assigned` | Queries from the address get this device's identity |
| `expired` | The device holds the newest claim, but the observation expired; queries get no identity from it |
| `ambiguous` | Another device claims the address with an equally new observation (or another DHCP scope leases it to a different MAC); neither identifies queries |
| `superseded` | `ownerSource`/`ownerId` holds a newer claim; for a lease, an inventory device's MACs win over the lease MAC |
| `withdrawn` | The device's own source later removed the claim; the older observation is not restored |

```sh
kudurru inventory
kudurru inventory --kind dhcp --stale false
kudurru inventory --search 02:00:5e -o json
```

On the complete SQLite runtime the API reads the same inventory snapshot that
serves DNS for that request; on Kubernetes it reads the replica's shared
inventory. DHCP leases are always the answering node's. Pages are cut by
source and ID; filters are `source`, `kind`, `stale` and `q`.

In the dashboard, **Filtering > Inventory** (also linked from the Clients
page) shows the same list with these filters, the address states in words,
and a link to the client page of every address the query log knows. A device
from an integration links to that Integration: `GET /api/v1/integrations`
names each incarnation's `inventorySource`, the source ID its devices carry.
On a member of a SQLite cluster the page says that DHCP leases and lease MACs
are the answering node's own: `GET /api/v1/inventory` answers
`nodeLocalLeases: true` there, so operators and auditors, who cannot read
`GET /api/v1/cluster`, get the caveat too. Sessions without `inventory.read` see that the
view is restricted, and the page sends no request for them.

Client IDs are optional, client-selected policy identifiers. They are **not
passwords or authentication**. Do not use them to grant privileged DNS updates
or API access. Use one ASCII DNS label of at most 63 characters: letters,
numbers and interior hyphens. IDs are case-insensitive.

For direct DoT or DoQ, configure a suffix and a certificate covering its client
names (typically a wildcard certificate):

```yaml
listeners:
  dot:
    - address: 0.0.0.0:853
      clientIDDomain: dot.example.com
      tls:
        certFile: /run/secrets/dns-cert.pem
        keyFile: /run/secrets/dns-key.pem
  doh:
    - address: 0.0.0.0:443
      path: /dns-query
      clientIDs: true
      tls:
        certFile: /run/secrets/dns-cert.pem
        keyFile: /run/secrets/dns-key.pem
```

Connecting to `phone.dot.example.com` with DoT selects `phone`.
`https://dns.example.com/dns-query/phone` selects the same ID with DoH.
DoQ uses the same suffix option; DoH3 uses the same path option. The ordinary
DoH path stays available without an ID. Extra or escaped path components are
rejected. The listener settings are host-only and require restart.

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata: {name: phones, namespace: default}
spec:
  clients:
    - clientID: phone
```

Specific IP, MAC, CIDR or hostname matches take precedence over this group.
Both ordinary and ID-specific DoH responses use private HTTP caching so shared
HTTP caches cannot reuse answers across client policies.

## Client IDs through the dnsdist edge

When the dnsdist edge terminates TLS, kudurru cannot read the original server
name or URL. The edge derives the ID itself, with the same rules as a kudurru
listener, and sends it to the backend in private PROXYv2 TLV `0xE1` (D389):

```yaml
edge:
  proxyProtocolSecretFile: /run/secrets/proxy_secret   # required for client IDs
  tlsDir: /run/secrets/edge-tls                         # covers *.dot.example.com
  clientIDDomain: dot.example.com                       # DoT and DoQ: phone.dot.example.com
  clientIDs: true                                       # DoH and DoH3: /dns-query/phone
```

The command-line forms are `kudurru edge run --client-id-domain dot.example.com
--client-ids`. With these settings, the same client ID selects the same client
group and policy whether the device talks to kudurru directly or through the
edge. Through the edge, `phone.dot.example.com` and `/dns-query/phone` select
`phone`. A server name with more than one label before the suffix carries no
ID. With `clientIDs` on, the edge answers HTTP 404 for every other path below
`/dns-query`, including empty, extra and escaped components, as a kudurru
listener does. A query that carries an ID never uses the edge's packet cache,
because that cache does not key on the client ID.

kudurru accepts TLV `0xE1` only from a peer in `listeners.proxyProtocolFrom`
that also sends the valid shared-secret TLV `0xE0`. This applies to loopback
edges too. The edge refuses to render client IDs without
`proxyProtocolSecretFile`. A forged TLV without the secret, or with a wrong
one, drops the query. Ordinary HTTP forwarding headers and DNS EDNS options
cannot set the ID. The settings are host-only and need a restart.

Known issue: the pinned dnsdist 2.1.3 edge never answers a DoQ or DoH3
request whose end of stream (the QUIC STREAM FIN) arrives in a separate,
empty frame after the query data. The client waits until its own timeout and
the query never reaches kudurru. This happens with and without client IDs.
A client that sends the FIN together with the query is answered, and kudurru's
own DoQ and DoH3 listeners answer both forms. Some QUIC client libraries send
the FIN separately now and then: in our tests a Go (quic-go) client did so for
roughly one DoH3 request in seven to one in fifteen on a fresh connection. If DoQ or DoH3 clients
behind the edge see occasional timeouts, check whether they send the FIN on
its own, or point them at a kudurru listener directly.
