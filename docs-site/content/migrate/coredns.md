---
title: Move from CoreDNS to kudurru
linkTitle: From CoreDNS
description: Preview the supported subset of a CoreDNS Corefile, review which plugins convert and which are reported, and apply a tracked migration.
weight: 23
---

Preview a Corefile with:

```sh
kudurru import coredns --file Corefile -n default -o json > preview.json
```

Review the objects, settings and loss report using the
[offline configuration guide](/migrate/offline-config/). Conversion reads only
the Corefile you name. It opens no zone, hosts or imported files, does not read
environment variables and contacts neither CoreDNS nor kudurru. Keep CoreDNS
serving DNS and retain its Corefile and zone files during review.

## Corefile syntax that is read

The reader follows the Corefile grammar documented for CoreDNS 1.12
([configuration manual](https://coredns.io/manual/configuration/)). It was
written from that description and does not use CoreDNS or Caddy code.

- Comments start with an unquoted `#` and end at the line break.
- Double-quoted tokens may hold spaces and line breaks; `\"` is a quote.
- A server block is one or more keys, `zone[:port]` with an optional scheme
  such as `dns://` or `tls://`, separated by spaces or commas, followed by
  `{`. A file with one server block may omit the braces. Keys may be domains,
  `.` or reverse prefixes on an octet (IPv4) or nibble (IPv6) boundary, such
  as `10.0.0.0/8`.
- Each directive starts a line; a trailing `{` opens its option block.
- `(name) { ... }` defines a snippet, and `import name` expands it in place.

Imports of file paths or globs, imports with arguments and recursive imports
are reported and omitted. A directive or key that contains an environment
placeholder (`{$NAME}` or `{%NAME%}`) is omitted with its value redacted.
The preview's warnings name this syntax version.

## What converts

| Corefile | kudurru |
|---|---|
| `forward . TO...` in the root (`.`) block | `upstreams.groups.default` |
| `forward` in another block, or `forward FROM TO...` | A named upstream group and `upstreams.conditional` routes for each zone |
| `forward` servers: IP, `IP:port`, `dns://`, `tls://` | The same addresses; `force_tcp` makes plain servers `tcp://` |
| `forward` `policy random`, `sequential`, `round_robin` | Strategy `random`, `strict`, `random` (reported) |
| Inline `hosts` entries (`IP name...`) | Exact-name rewrites in `Policy/default`, with PTR answers unless `no_reverse` (see [server block routing](#server-block-routing)) |
| `rewrite name [exact] FROM TO [answer auto]` | A CNAME rewrite from FROM to TO in `Policy/default` |
| `cache [TTL]` with `success`, `denial`, `prefetch`, `serve_stale` in the root block | `cache.maxTTL`, `cache.negativeTTL`, `cache.minTTL`, `cache.maxEntries`, `cache.prefetch.*`, `cache.serveStale.ttl`. TTLs must be 1 to 2,147,483,647 seconds (a minimum TTL may be 0) and capacities 1 to 1,073,741,824; any other value omits the `cache` directive as a unit |
| `acl` in the root block: `block type T...`, then `allow net N...`, then a final `block` | `control.blockedQTypes` and `control.allowedNetworks` |

## What is reported and not converted

- Server block keys for `tls://`, `https://`, `https3://`, `grpc://` and
  `quic://`, with their certificates; each block is reported with its keys.
  A block that also has plain DNS keys converts for those keys only.
  Configure the kudurru DoT, DoH and DoQ listeners instead. Listener ports are
  not imported.
- `file` and `auto` zones: the loss report names each zone. Import the zone
  file separately with `kudurru import bind ZONE -f FILE`. `secondary` zones
  are named too; create a secondary `DNSZone` for each.
- The hosts file itself (by default the system hosts file); only inline
  entries convert. Without `fallthrough`, CoreDNS answered SERVFAIL for other
  names in the hosts zones; kudurru forwards them.
- A `forward` with `tls_servername`, a `tls` client certificate, `except` or a
  resolv.conf file is omitted as a unit. Rewrite such a forwarder as
  `tls://NAME` servers by hand. Health-check, failure and connection options
  are noted; kudurru uses its own upstream health tracking.
- Other `rewrite` forms (prefix, suffix, substring, regex, answer rules, type,
  class, EDNS0, TTL, CNAME and rcode rewrites) and `continue` chains.
- `cache` or `acl` scoped to zones other than the root, more than one `acl`, and
  `acl` rules in any other order or with `filter` or `drop`. kudurru always
  answers loopback clients.
- Every other plugin, for example `errors`, `log`, `health`, `ready`,
  `prometheus`, `reload`, `loop`, `loadbalance`, `kubernetes`, `template` and
  `dnssec`, is named per server block. Directive names that are not CoreDNS
  plugins are counted with the name redacted.

The same zone in two plain DNS server blocks (for example on two ports) is
refused as ambiguous; convert one listener at a time.

## Server block routing

CoreDNS sends each query to the plain DNS server block with the longest
matching zone, so a block's `hosts` and `rewrite` answers apply only to names
that reach that block. The importer follows the same rule: a `hosts` name, a
`rewrite name` source or a `forward FROM` zone that falls inside a more
specific server block (for example `nas.home.example` in the root block when a
`home.example` block exists) is omitted, and the report counts it and names the
more specific block's zone. A `hosts` PTR answer is kept only when its reverse
name reaches the same block: hosts entries in an `example.org` block get no PTR
answers, and the root block's PTR answers for `10.0.0.0/8` are omitted when a
`10.0.0.0/8` block exists. Each omission is counted in the report, with names
redacted.

Hosts and rewrite answers replace a destination `Policy/default`, including
its block lists. They are answered locally with the `response.ttl` setting
and are limited to 128 per policy; above that limit, all of them are omitted
and reported. An exact name rewrite answers with a CNAME to
the target, while CoreDNS answered under the original name.

Apply only reviewed selections with `--apply --wait`, inspect the report with
`kudurru import runs RUN_ID`, and follow the
[cutover and rollback checklist](/migrate/cutover/).
