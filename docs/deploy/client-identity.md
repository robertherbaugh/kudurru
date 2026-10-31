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

When an edge terminates TLS, the backend cannot read the original SNI or URL.
Backend support accepts a normalized client ID in private PROXYv2 TLV `0xE1`
only with a trusted transport peer and valid shared-secret TLV `0xE0`; even
loopback ID propagation requires the secret. Ordinary HTTP forwarding headers
and DNS EDNS options cannot set it. Edge rendering and packet-cache acceptance
must be enabled together before relying on this forwarding path.
