# Client identity and transport IDs

Client groups can match exact IPs, MAC addresses, CIDRs, hostnames, hostname
globs or client IDs. The first matching tier selects all matching groups:
exact IP → MAC → longest nonzero CIDR → hostname → name glob → client ID →
catch-all `/0` → default. Inventory supplies MACs and hostnames from memory.
Expired inventory stops identifying a device, and reassigned IPs never inherit
an older device's policy. SQLite can populate inventory through the background
[UniFi integration](integrations.md); its management UI and Kubernetes execution
remain separate work. Private group contributions retain their original source
and expiry, while manual public matchers keep their configured behavior.

## Where each identity comes from

A matcher matches only when kudurru has that identity for the query:

| Matcher | Source | What it needs |
|---|---|---|
| `ip`, `cidr` | The source address of the query, or the address in an authenticated PROXYv2 header from a trusted edge | Nothing more |
| `clientID` | The server name of a DoT or DoQ connection, or the path of a DoH or DoH3 request | The listener settings on this page |
| `mac` | An inventory integration that reports the device with its addresses | A running integration: [integrations.md](integrations.md) says on which deployments integrations run |
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
The built-in DHCP server supplies names to these matchers, not MAC addresses.

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
