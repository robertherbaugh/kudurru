# Dynamic DNS updates (RFC 2136)

A primary zone can accept RFC 2136 UPDATE messages, the protocol of `nsupdate`,
of DHCP servers that register leases, and of the `rfc2136` providers of ACME
clients and external-dns. Updates are off by default. A zone accepts them only
from TSIG keys that you name, and only for the names and record types that you
grant to each key.

Both store drivers accept updates. They keep the records in different places:
see [SQLite](#sqlite) and [Kubernetes](#kubernetes).

## Before you start

- A primary zone. Secondary, stub and forward zones do not accept updates.
- A TSIG key (HMAC-SHA256 or HMAC-SHA512) that every node knows: a file key
  from `tsigKeys` in the node configuration, or a
  [stored TSIG key](tsig-keys.md).
- Clients that reach a node over plain DNS on UDP or TCP, or over DNS over TLS.
  An update that arrives over DoH, DoQ or DoH3 is refused.

## Create a key

Generate a secret and keep it in a file that only you can read:

```sh
umask 077
tsig-keygen -a hmac-sha256 ddns-key > ddns-key.conf   # for nsupdate -k
```

The `secret` line of that file holds the base64 secret. Give the same secret
to kudurru in one of two ways.

A file key, the same on every node (needs a restart):

```yaml
tsigKeys:
  - name: ddns-key
    algorithm: hmac-sha256
    secretFile: /etc/kudurru/ddns-key.secret   # only the base64 secret
```

Or a stored key, created once through the API and used by every node without
a restart ([tsig-keys.md](tsig-keys.md)). The CLI reads the secret from a
private file so that it is not a command argument:

```sh
kudurru tsigkeys create ddns-key --algorithm hmac-sha256 --secret-file /etc/kudurru/ddns-key.secret
kudurru tsigkeys list          # names and algorithms, never secrets
```

## Allow updates for a zone

Set `spec.policy.update` to `allow` and add one grant for each key, name and
set of types in `spec.policy.updatePolicies`:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata:
  name: home
spec:
  type: primary
  zoneName: home.example.
  nameservers: [ns1.home.example.]
  policy:
    update: allow
    updatePolicies:
      - tsigKeyRef: ddns-key
        name: "*.home.example."
        types: [A, AAAA]
      - tsigKeyRef: ddns-key
        name: _acme-challenge.www.home.example.
        types: [TXT]
```

Apply it with `kudurru apply -f zone.yaml`, with
`kudurru zones options get home -o yaml` and `kudurru zones options set`, as an
object file, or with `kubectl apply` on Kubernetes. The dashboard does not edit
these two fields.

| Field | Meaning |
|---|---|
| `update` | `deny` (the default) accepts no update. `allow` accepts updates that a grant covers. |
| `updatePolicies[].tsigKeyRef` | The name of the TSIG key, with or without the final dot. A grant for a key that the node does not have gives nothing. |
| `updatePolicies[].name` | The owner name the key may change: one exact name, or `*.` followed by a name for every name below it. `*.home.example.` covers `nas.home.example.` and `a.b.home.example.`, but not `home.example.` itself. The name must end with a dot and must be inside the zone. |
| `updatePolicies[].types` | The record types the key may change at those names, for example `[A, AAAA]`. Each entry is the name of a record type, or `TYPE` followed by its number (`TYPE65280`). An empty list or `ANY` means every type. |

`allow` without a grant accepts nothing. A zone holds at most 4,096 grants.

On SQLite the API refuses a policy that kudurru cannot use, also on a zone
that denies updates: it answers `invalid`, names the field, and stores
nothing, so the zone keeps its previous policy. It refuses

- an `update` other than `deny` or `allow`. The schema also has
  `allowOnlyPrivate`, `allowOnlyZoneNameServers`, `useNetworkACL` and
  `zoneNameServersAndACL`, which kudurru cannot use for updates;
- a grant `name` without the final dot (`"*.home.example"`), outside the
  zone, or with a `*` anywhere but as the whole first label
  (`_acme-challenge.*.home.example.`, `*.*.home.example.`);
- an entry of `types` that is not a record type an update can carry, such
  as `BOGUS`, `OPT` or `AXFR`;
- an empty or malformed `tsigKeyRef` (`""`, `ddns..key`);
- more than 4,096 grants.

A grant for a well-formed key name that the node does not have is accepted
and gives nothing. The Kubernetes store does not run this check: such a
policy can be stored there, the zone keeps answering, and every update for
it is answered `REFUSED`.

## Send an update

```sh
nsupdate -k ddns-key.conf <<'EOF'
server 10.0.0.11
zone home.example.
update delete printer.home.example. A
update add printer.home.example. 300 A 192.168.1.20
send
EOF
dig @10.0.0.11 printer.home.example. A +short
```

An update is applied completely or not at all: one record that is not
allowed refuses the whole message. kudurru checks the prerequisites of an
update against the whole zone, including the records that come from record
sets. An accepted update is answered `NOERROR` with a TSIG signature. On SQLite
the answer is sent after the node has stored the change and serves it. On
Kubernetes it is sent after the change is written; every replica serves it
as soon as it has seen the change. The SOA serial of the zone advances, and
the zone's NOTIFY settings apply as for any other change.

One message holds at most 4,096 records and 65,535 bytes.

## What an update may change

Records that come from somewhere else stay under the control of that source.
An update is refused when it touches:

- the SOA record and the NS records at the zone apex;
- a name and type that a record set provides, and every type at a name where
  a record set provides a CNAME;
- names below a DNAME that a record set provides.

A deletion of a whole name (`update delete printer.home.example.`) needs a
grant for every type that exists at the name. If one type is not granted,
nothing is deleted.

## Answers

| Answer | Meaning |
|---|---|
| `NOERROR` | The update is accepted, or it changed nothing. |
| `NOTAUTH` | The node does not serve this zone as a primary zone. |
| `NOTAUTH` with the TSIG error `BADKEY`, `BADSIG` or `BADTIME` | The signature was not accepted: the node does not have the key, the secret differs, or the clocks differ. |
| `REFUSED` | The message is not signed or arrived over an unsupported transport, a grant of the zone names the key but no grant covers the name and type, or the record belongs to another source. On Kubernetes also: the zone does not allow updates, or no grant of the zone names the key. |
| `YXDOMAIN`, `NXDOMAIN`, `YXRRSET`, `NXRRSET` | A prerequisite of the update did not hold. Nothing changed. |
| `NOTZONE` | A record of the update is outside the zone. |
| `FORMERR` | The message is malformed or too large. |
| `SERVFAIL` | The node could not store or publish the change. On SQLite, a signed update is also answered `SERVFAIL` when the zone does not allow updates, or when the node has the key but no grant of the zone names it (also `allow` without a grant). An update that was stored can answer `SERVFAIL` when its publication failed: query the name before you send the update again. |

On SQLite, when every update of a client is answered `SERVFAIL`, compare the
key name that the client uses with `tsigKeyRef` in the zone's grants before
you look for a storage problem. An update with a key that no grant names
stores nothing.

With BIND's `nsupdate`, a wrong key name or a wrong secret looks like a clock
problem. It prints `TSIG error with server: clocks are unsynchronized` before
`update failed: NOTAUTH(BADKEY)` or `NOTAUTH(BADSIG)`. Check the key first.

## SQLite

A SQLite node keeps dynamic records in the zone's stored state in `state.db`,
together with the records compiled from record sets. They are not objects:

- `kudurru records list <zone>`, the zone page of the dashboard and
  `kudurru zones export <zone>` show them together with the other records of
  the zone. `kudurru get rrsets` does not show them.
- `kudurru backup` writes objects only and does not contain them. A copy of
  the data directory of the stopped node does. Use `kudurru zones export` for
  a readable copy.
- A change to a record set does not remove dynamic records, and they are still
  there after a restart.

### Enrolled SQLite cluster

In an [enrolled cluster](sqlite-cluster.md) only the primary applies
updates, but a client may send them to any member. A follower checks that it
serves the zone as a primary zone and verifies the signature with its own copy
of the key, then passes the update on to the primary over the member
connection. The primary checks the key again, applies its own update policy
and prerequisites, and commits. The follower returns the primary's answer
(`NOERROR`, `NXRRSET`, `REFUSED` and so on), signed with the request's key.
The key must have the same name, algorithm and secret on both nodes. Stored
keys replicate; a key in the configuration file must be in the file of each
node. Followers serve the dynamic records after their next pull.

When the follower cannot reach the primary, or loses the connection after
sending, it answers `SERVFAIL` and does not retry. RFC 2136 tells a client
to try its next server on `SERVFAIL`, so list the primary as well. A lost
exchange may still have committed on the primary; check the record before
sending a change that is not idempotent again. A fenced or conflicted node
answers `SERVFAIL` without forwarding.

A forwarded update expires at most four seconds after the follower sends it,
and the primary checks that against its own clock. If the primary's clock
runs four seconds or more ahead of a follower's, that follower answers
`SERVFAIL` for every update it passes on, so keep member clocks within a
second of each other (see the clock requirement in
[the cluster guide](sqlite-cluster.md)).

During a burst of updates, for example from a DHCP server, each commit is
followed by a new publication on the primary and, at the next pull, on the
follower. An update that reaches either node while that publication is being
replaced waits for it, within the four seconds of the forward, instead of
failing: the follower selects the zone and checks the key again on its new
publication, and the primary checks again on its own. Nothing has been sent
or applied at that point, so this is not a retry of the update. A burst sent
through a follower no longer fails because of these publications. An update
through a follower can still be answered `SERVFAIL` when:

- it could not be sent, or was not answered, within the four seconds of the
  forward (a publication that takes longer, on a loaded node, counts here);
- the follower cannot reach the primary, or the primary does not accept the
  follower as a member at that moment;
- the primary committed the update but could not publish the commit (the
  update is applied; check it before a change that is not idempotent);
- the zone is retiring on the primary.

In every case the follower sends the update at most once. An insert or
delete of a whole RRset is idempotent, so a client's retry of it is safe; for
any other change, check the record first when the answer may follow a
commit. An update sent to the primary directly does not wait: right after a
commit it can still be answered `SERVFAIL` without being applied, and the
client's retry is safe.

Static twins do not replicate: an update changes only the node that received
it.

## Kubernetes

Kubernetes has no transaction over several objects, so each zone has exactly
one record set that updates own: the `DNSRecordSet` named `rfc2136-` followed
by the name of the `DNSZone` object (`rfc2136-home` above), marked
`kudurru.io/managed-by: dns-update`. It is in the release namespace, or in the
first namespace of the zone's `recordSetSelector` when that selector does not
admit the release namespace. kudurru creates it with the first accepted update,
rewrites it as a whole for every later one and deletes it when the last
dynamic record is removed.

- Every replica accepts updates. On one replica, updates for a zone wait for
  each other. Between replicas, the replica whose write loses reads the zone
  again and plans the update again, up to eight times within ten seconds,
  and then answers `SERVFAIL` without a write. A client can send that update
  again.
- Do not create, edit or delete this record set yourself, and do not manage it
  with GitOps. If an object with that name exists and is not marked as owned by
  `dns-update`, every update for the zone is answered `REFUSED`. The API
  refuses changes to the object because it has another owner.
- If the zone's `recordSetSelector` has labels, kudurru copies them to the
  record set. A selector that still does not admit the record set makes every
  update answer `REFUSED`.
- Only record types that kudurru can write as a record set target are
  accepted; other types are answered `REFUSED`.
- `kudurru backup` contains the record set like every other object, but
  without its owner mark, and `kudurru restore` creates it in a new cluster as
  an object owned by the API. Updates for that zone are then answered
  `REFUSED`. Delete the restored record set
  (`kudurru delete rrset rfc2136-home -n <namespace>`) and let the clients
  register again, or keep it and stop using updates for the zone.
