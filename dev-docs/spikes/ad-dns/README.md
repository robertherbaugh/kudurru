# AD DNS secure-registration feasibility probe

Started 2026-10-22 for [D222](../../design-record.md#16-decision-log) and
[AD6 in the functional outline](../../active-directory-dns.md).

The probe establishes a Kerberos security context through RFC 2930/3645
TKEY, verifies GSS-TSIG requests, and signs replies. Every UPDATE returns
REFUSED, including authenticated requests. Authentication alone does not
grant record ownership or implement atomic, durable DNS changes.

This is a separate Go module, excluded from the kudurru binary and the
root module's dependency graph. No production settings, API, CRDs, query
path, import behavior or website support claims change.

## Run the checks

From the repository root, with Go 1.27 and a C compiler for the race detector:

```sh
make ad-dns-check
```

For the independent interoperability test, install MIT Kerberos server and
client tools (`krb5kdc`, `kdb5_util`, `kadmin.local`, `kinit`) and BIND's
`nsupdate`. On Amazon Linux:

```sh
sudo dnf install -y make gcc krb5-server krb5-workstation bind-utils
KUDURRU_AD_MIT_TEST=1 make ad-dns-check
```

The test creates a disposable realm, random master key, service and member
keytabs, credential cache, and loopback KDC on an ephemeral port. It uses
neither the host's default realm nor an existing AD forest, stops its
processes, and removes the temporary files afterward. It does not need root.
The opt-in test fails if a required tool is missing; without the opt-in,
the unit and network failure tests run and the MIT test is skipped.

Additional parser and MIC checks, from this directory:

```sh
go test -run '^$' -fuzz '^FuzzAcceptor$' -fuzztime=20s -parallel=2
go test -run '^$' -fuzz '^FuzzMIC$' -fuzztime=20s -parallel=2
```

## Protocol behavior and evidence

| Path | Evidence obtained on 2026-10-22 |
|---|---|
| Independent Kerberos and DNS client | MIT Kerberos 1.21.3 and BIND `nsupdate` 9.18.50 establish SPNEGO/Kerberos contexts, send GSS-TSIG updates, and verify signed REFUSED replies over TCP and UDP. |
| Build | Go 1.27.2, Linux amd64, `CGO_ENABLED=0`; the probe binary is statically linked. |
| Checks | Race tests and vet pass. Short fuzz runs exercised over 470,000 inputs without a failure. Dependency license checks pass; `govulncheck` reports no reachable vulnerabilities. |
| Ticket identity | Exact service principal and realm; AES tickets and context keys only; ticket/authenticator checks and AP-REQ replay detection use gokrb5. |
| Context negotiation | Mutual authentication, integrity and replay protection are required. Raw Kerberos and optimistic SPNEGO with Kerberos first are accepted; no NTLM, delegated credentials, channel binding or mechanism-list MIC negotiation. |
| Message integrity | Tampering, wrong direction/flags, expired contexts, old and duplicate MICs are refused; a failed MIC cannot advance the receive window. Concurrent copies authenticate at most once. |
| Resource bounds | Explicit client CIDRs; at most 128 contexts globally and eight per authenticated principal, a one-hour context lifetime, 32 KiB input tokens, bounded DNS record counts and TCP deadlines. Ticket verification does not hold the context-map lock. |
| Write safety | Unsigned and authenticated updates are refused. A verified identity is observable without logging tokens or keys. No record store is attached. |

The receive window tolerates reordering within 64 messages when only replay
protection is requested; negotiated sequence service requires strict order.
This probe does not implement multi-round SPNEGO,
TKEY context deletion, retransmission response caching, keytab hot reload,
or a production AP-REQ replay-cache budget. Contexts and replay state are
process-local. GSS failure responses use BADKEY; BADTIME includes the
server time. UDP negotiation with large real Windows PACs and truncation
still needs qualification. These limits preclude shipping it as the AD
acceptor unchanged.

## Run against a disposable lab

Use a unique DNS service principal and a keytab provisioned by the realm
administrator. The exact principal must match the server name selected by
the client's SOA discovery, including any alias; a DNS service keytab must
not be committed or put in the command line as inline key material.

```sh
bin/ad-dns-probe \
  -listen 192.0.2.53:15353 \
  -zone gssprobe.corp.example \
  -principal DNS/probe.corp.example@CORP.EXAMPLE \
  -keytab /run/secrets/ad-dns.keytab \
  -allow 192.0.2.0/24
```

The default listen address and allowed clients are loopback. The probe
serves minimal SOA discovery for the disposable zone; the SOA primary is
the hostname in `-principal`. It resolves no other records and forwards
nothing. Direct only disposable test registrations to it. Do not change
production DC NIC/DHCP resolver settings for this experiment.

After obtaining a lab member's Kerberos credentials, use `nsupdate -g`
with an explicit server, port and zone. Expected evidence is a
`context established` event followed by `authenticated update refused`
with the member's principal, and a verified REFUSED reply at the client.
A REFUSED reply alone proves nothing about authentication; the automated
test requires both client and server evidence. Avoid client debug/trace
options when collecting shared logs because they can include tokens.

## Dependency evaluation

| Candidate | License | Role and finding |
|---|---|---|
| `jcmturner/gokrb5/v8` v8.4.4 | Apache-2.0 | Pure-Go Kerberos ticket/authenticator, replay, crypto and RFC 4121 MIC primitives. Used experimentally here; its SPNEGO service does not generate the complete mutual-authentication exchange required by DNS, so this probe assembles AP-REP from the protocol structures. This is not a final production dependency decision. |
| `jcmturner/gofork` v1.7.6 | BSD-3-Clause | ASN.1 structures and encoding used by gokrb5. |
| `miekg/dns` v1.1.73 | BSD-3-Clause | DNS/TKEY/TSIG wire format and transport; already used by kudurru. Its default message acceptance rejects UPDATE before signature handling, so the probe explicitly allows bounded UPDATE packets. |
| `bodgit/tsig` (client) and `bodgit/gssapi` v0.0.4 | BSD-3-Clause | Evaluated from documentation and permissive source; not dependencies. The TSIG package supplies a client. The wrapper's acceptor needs further replay, malformed-checksum and keytab-selection review before production use. |

No copyleft source is copied or linked. MIT Kerberos and BIND are separate
test processes. The probe uses the Kerberos standards, gokrb5's exported
primitives, and kudurru's existing TSIG integration pattern.

## Next gates

1. Obtain a real Windows lab and test DC Netlogon, member and DHCP
   registrations, SPNEGO variations, large PACs, AES policy, SPN aliases,
   KVNO/key rotation, clock skew and expired-context recovery.
2. Select and harden the production acceptor after those results, including
   replay budgets, UDP/TCP retries, TKEY deletion, context eviction and edge
   routing/failover. Keep accepted identities separate from authorization.
3. Implement RFC 2136 prerequisites and atomic zone mutation, then
   principal/name/value ownership and durable writes for each deployment
   mode. Connect secure registration only after those semantics are tested.
4. Add assessment/import, ownership handoff, aging, DHCP interoperability,
   administration, rollback, and the full Windows acceptance matrix from
   the functional outline.

There is no Windows lab yet. The MIT test is evidence for protocol
feasibility; Windows compatibility and complete AD DNS replacement remain
unverified.
