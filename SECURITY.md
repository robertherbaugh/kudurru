# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately by email to
[security@kudurru.io](mailto:security@kudurru.io).
Do not open a public issue, pull request or discussion for a suspected
vulnerability.

Include what you can of:

- the affected version (`kudurru version`) and deployment shape (single
  Docker instance, Compose HA with dnsdist and keepalived, Kubernetes chart);
- the component (DNS listeners, REST API and dashboard, authentication,
  admin socket, internal listener, edge supervisors, list fetcher, query
  log, external-dns webhook);
- a reproduction (requests, configuration excerpt with secrets removed,
  logs) and the impact you observed.

What to expect:

- an acknowledgement within 5 working days;
- an assessment (accepted, needs more information, or not a vulnerability
  with the reasoning) within 14 days;
- a fix in a patch release of every supported version, a GitHub security
  advisory and a CVE when the issue is accepted. We coordinate the
  disclosure date with you and credit you in the advisory unless you prefer
  otherwise. The default embargo is 90 days from the report, shorter when a
  fix ships earlier, longer only by agreement.

kudurru is maintained by volunteers; there is no bug bounty.

## Supported versions

| Version | Supported |
|---|---|
| latest v0.1.x release | security fixes |
| `main` (unreleased) | fixed on main, no backports |
| anything older | no; upgrade to the latest release |

Before v1.0 only the latest minor release line receives fixes. The
dnsdist and keepalived images under `images/` follow the same rule; the
pinned upstream versions are bumped by the CVE job.

## Threat model (summary)

The design and its rationale live in [dev-docs/design-record.md](dev-docs/design-record.md)
§3.3 (dataplane), §8 (authentication, RBAC, tokens, bootstrap) and §14
(risks). The M5 security review is in [dev-docs/milestones/m5/security.md](dev-docs/milestones/m5/security.md),
the review of the v0.1 round (block page, unblock requests, allowlists,
catalogue fetchers, release pipeline) in
[dev-docs/milestones/m5/security-v0.1-round.md](dev-docs/milestones/m5/security-v0.1-round.md).

### Assets

- The answers kudurru gives (authoritative zones, blocking policy,
  forwarding): an attacker who changes them can redirect every client on
  the network.
- The configuration objects in the store (zones, records, block lists,
  policies, client groups) and the identity objects (local users, API
  tokens, service accounts, role bindings, identity providers).
- `KUDURRU_ENCRYPTION_KEY`: it seals session cookies, derives the internal
  listener's token and CA, and is the key for secrets kudurru will store
  encrypted (v0.2). Anyone holding it can act as any session or node.
- The query log: client addresses and the names they resolve are personal
  data; `querylog.privacy` modes are applied at ingest and are irreversible.
- The license signing key (licensor only): whoever holds it can forge
  license keys that every release trusting its kid accepts, and keys cannot
  be recalled. It lives in Infisical and, at run time, only in the memory of
  the license portal (`license-server/`), never in its database or image.
- The license portal's database: customers, contact addresses, issued keys,
  non-commercial attestations and the audit trail of issuing.

### Trust boundaries

| Boundary | Who is on the other side | Controls |
|---|---|---|
| DNS listeners (Do53, DoT, DoH, DoQ, DoH3) | any client that reaches them | per-prefix rate limit with TC slip on UDP (TCP and TLS transports prove the source), message sanity checks, body/header caps on DoH, query and transfer ACLs, TSIG (HMAC-SHA256/512, constant-time compare, time window) for transfers and NOTIFY |
| PROXYv2 from dnsdist | hosts in `listeners.proxyProtocolFrom` | headers are honoured only from listed peers **and** only with the shared 32-byte TLV `0xE0` secret (constant-time compare); source-CIDR trust alone is never enough, because any host in a pod CIDR or LAN /24 could forge client addresses |
| REST API and dashboard (`listeners.api`) | browsers and API clients | authentication chain (API tokens stored as SHA-256 with constant-time compare and no prefix oracle, OIDC ID tokens with audience/azp checks, Kubernetes ServiceAccount tokens with audience `kudurru`, sealed AES-256-GCM session cookies with idle and absolute expiry and `sessionsValidAfter` revocation), per-route RBAC declarations with zone scopes, CSRF (`http.CrossOriginProtection` plus the `X-Kudurru-CSRF` header for cookie writes), login lockouts per client and per user, Argon2id password hashing, security headers (CSP, frame denial, nosniff, HSTS when TLS terminates in kudurru), request body caps |
| Admin socket (`/run/kudurru/kudurru.sock`) | local processes | `SO_PEERCRED`: uid 0, the server's own uid or `auth.adminSocket.group`; mode 0660. A process that passes is the break-glass local root |
| Internal listener (`listeners.internal`, :8082) | other kudurru nodes | TLS 1.3 only with node certificates from a CA derived from the encryption key, a bearer token derived the same way (constant-time compare), per-address rate limit, no request bodies; peers get read-only grants |
| Kubernetes apiserver | the cluster | SubjectAccessReview for ServiceAccounts (no `impersonate` grant), JWKS fetched through the apiserver, TokenReview only while discovery fails |
| Identity providers | the OpenID Provider | authorization code + PKCE (S256), single-use sealed pre-auth cookie with state and nonce, `next` redirects restricted to same-origin paths, strict provisioning (no login without a binding, never an automatic admin), IdP usernames and groups prefixed `<idp>:` |
| Block list and catalogue URLs | the operators who write BlockList objects; the admins who set `blocking.catalogue.url` and `adguardServicesURL`; whoever serves those documents | the fetching node refuses loopback, link-local (cloud metadata), unspecified and multicast addresses unless `blocking.allowLocalListURLs` is set, on the resolved address of every connection and on every request URL, redirects included; bodies are size-capped after decompression and validated (catalogue entry counts, AdGuard rules limited to single sites); replicas pull bodies by SHA-256 digest |
| Block page (`listeners.blockPage`, HTTP and HTTPS) | any device that reaches it, unauthenticated | a fixed template with every value escaped, no script or cookies, a CSP that loads nothing and forbids framing; never proxies or fetches; per-client rate limits. The unblock request form refuses cross-site posts, caps the body and fields, and is bounded per client, per node (30 writes a minute) and by `unblockRequests.maxPending`; the client is recorded as `querylog.privacy` allows, under an object name keyed by the encryption key |
| external-dns webhook | external-dns | **unauthenticated by design** (the external-dns webhook contract has no authentication): it listens on `127.0.0.1:8888` by default; a wider listener must be restricted with a NetworkPolicy or firewall, and `externalDNS.webhook.zoneSelector` limits the zones it can write |
| dnsdist and keepalived | the supervisors that render their configuration | separate programs in their own images (never linked); rendered values are validated to characters that need no quoting; the dnsdist console and webserver listen on loopback with per-start credentials |
| License portal (`license-server/`, the licensor's service; kudurru nodes never contact it) | customers, license admins, the kudurru.io form (unauthenticated) | OIDC code flow with PKCE, verified ID tokens and server-side sessions; CSRF origin check and token; least-privilege roles and per-table database grants; the public form limited per network and per address, a honeypot and email confirmation, the key only by mail; issuing quotas; every change audited append-only; the signing key from a secret at run time (`license-server/README.md`, "Security notes") |

### Roles

`viewer`, `auditor`, `operator`, `zone-admin` (zone-scoped) and `admin`
(dev-docs/design-record.md §8.5). Operators can change filtering for every client
and point block lists at any URL that is not node-local; zone-admins control
only the zones they are bound to; only admins change settings, identities
and cluster membership. API tokens act as their owner, optionally narrowed
by scopes, and can never mint a broader or longer-lived token.

### Out of scope

- Attackers who hold `KUDURRU_ENCRYPTION_KEY`, root on a node, or write
  access to kudurru's Secrets, ConfigMaps or state.db.
- Denial of service by volumetric floods that exceed what a single node can
  absorb (front kudurru with dnsdist dynamic blocks or upstream filtering).
- Spoofed UDP sources exhausting a victim prefix's rate-limit bucket (the
  TC slip lets real clients retry over TCP).
- Findings that need an administrator to configure something the
  documentation warns against (for example `auth.session.secure: never`
  on an untrusted network, an unauthenticated webhook on a public address,
  or an identity provider whose `preferred_username` users can change).

## Hardening checklist for operators

- Serve the API over TLS (a certificate, `tls.auto: selfsigned`, or a
  TLS-terminating proxy listed in `listeners.api.trustedProxies`).
- Set `KUDURRU_ENCRYPTION_KEY` from a secret store and rotate it with
  `KUDURRU_ENCRYPTION_KEY_PREVIOUS`.
- Change the generated first-boot admin password; prefer OIDC for people
  and scoped, expiring API tokens (`auth.tokens.maxLifetime`) for
  automation. A password change or reset ends the user's sessions but not
  their API tokens: delete the tokens of a compromised account, or disable
  it.
- Grant `unblockrequests.decide` only to people you would let edit the
  Allowlist: an approval writes the allow list and attaches it to the
  policies by itself.
- With OIDC, bind groups rather than `User` subjects, or map the username
  to an immutable claim (`claimMappings.username.claim: sub`): the default
  chain starts at `preferred_username`, which some identity providers let
  users edit (docs/deploy/helm.md).
- Keep `listeners.proxyProtocolSecretFile` set whenever PROXYv2 is enabled.
- Keep the external-dns webhook on loopback or behind a NetworkPolicy.
- Pick a `querylog.privacy` mode that matches your obligations; it cannot
  be applied retroactively.
