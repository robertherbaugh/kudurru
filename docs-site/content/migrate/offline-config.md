---
title: Preview an offline DNS configuration
linkTitle: Offline configuration
description: Convert supported AdGuard Home or Blocky YAML settings locally, review losses and selections, and apply through a tracked migration.
weight: 25
---

In v0.2, `kudurru import adguard` and `kudurru import blocky` convert a
supported subset of a saved configuration into a reviewable object/settings
plan. This does not import a whole product or its working directory losslessly.
Keep the source serving DNS and retain its backup until cutover tests pass.

## Preview before applying

```sh
kudurru import adguard --file AdGuardHome.yaml -n default -o json > preview.json
kudurru import blocky --file config.yaml -n default -o json > preview.json
cat config.yaml | kudurru import blocky --file - -o yaml
```

The default and `--dry-run` only convert and print the plan, including a bounded
loss report in `warnings`. They do not contact either DNS server or write
configuration. The converter never reads referenced paths, downloads lists,
resolves client hostnames, or imports passwords, authentication hashes,
certificate/key paths, listener privileges, DHCP, users or query history.
An input must be one YAML mapping of at most 4 MiB. Duplicate keys, anchors,
aliases, merge keys, custom tags, excessive depth/node/scalar sizes and
ambiguous supported identities are rejected without echoing source values.

The plan records the source type without a filename or URL. Credential-bearing
URLs with userinfo, queries or fragments are omitted. Resolver stamps and
nonstandard DoH paths require manual conversion. Imported list URLs must be
public URLs without embedded credentials; inspect their paths before sharing a
preview. URL bodies are not inspected offline, so check destination fetch,
unsupported and invalid-rule status after applying. Conversion warnings never
include omitted values. A loss report is limited to 96 messages plus a notice
when further losses were omitted; a truncated report is not a complete inventory.

The 4 MiB source limit is separate from destination limits. The generated JSON
plan must fit the public API's 32 MiB request bound; the CLI rejects an oversized
plan before printing a preview or contacting the destination. Selection retains
unselected plan entries and does not reduce this payload. The server also bounds
the current migration to 1,000 objects and reserves space in its 1 MiB journal
before writes; a small source or preview does not guarantee destination admission.
Reduce the source configuration into separately reviewed imports if needed.
Refused import-start requests are never automatically replayed.

Choose a namespace explicitly with `-n`, or use the saved context namespace. Names are stable across repeated
previews; input file paths and map iteration order do not determine identities.
Applying may replace existing objects with the same names, including
`Policy/default`, and merge imported upstream groups into destination settings.

## Supported conversion and reported losses

| Source | Supported subset | Reported omissions or behavior to review |
|---|---|---|
| AdGuard Home | Current `filtering` section and older filtering fields under `dns`; filter/allow-filter URL references with enabled state; simple suffix and RE2 rules with global exceptions; IP/CIDR persistent clients, filtering and query-log overrides; one upstream per client; bootstrap IPs; one default upstream or two in parallel mode | Modifiers, complex/ambiguous custom rules, custom block responses, remote service protection, safe search, catalog IDs, temporary protection deadlines, client tags/lookup identifiers, file upstreams, conditional syntax and other multi-upstream selection modes require manual review. Unrecognized sections/settings are counted with values redacted. |
| AdGuard local records | Exact IP/CNAME rewrites within explicitly selected `--local-zone` suffixes; authority generated through the normal zone helpers | Wildcard and A/AAAA passthrough rewrites are omitted. Selected suffixes become authoritative; missing names stop forwarding. Record TTL is 300 seconds, SOA/NS are generated, and local answers bypass filtering. |
| Blocky | Groups-based upstreams with `strict`, `parallel_best` or `random` strategy and timeout; numeric/plain-IP, `tcp+udp`, DoT, DoH and DoQ endpoints; plain conditional mappings; URL and literal-block hosts/domain/wildcard/RE2 lists; IP/CIDR list assignments and matching upstream selectors; exclusive allow mode; block type and TTL | Hostname/glob/FQDN clients, resolver stamps/TLS name overrides, local list paths, query rewrites, conditional fallback, schedules, response-IP/CNAME checks and source loading semantics require review. A policy selecting a partially omitted or scheduled list group receives no filtering references, with an explicit warning; rebuild it before applying. |
| Blocky local records | `customDNS.mapping` IP lists with `filterUnmappedTypes: true`, unit-bearing TTLs or integer minutes | Each mapping becomes an authoritative suffix zone with apex and wildcard IP records. Generated SOA/NS add answers; automatic reverse records are omitted. Unmapped-type forwarding, query rewriting and zone-file extensions are omitted. Local answers bypass filtering. |

Client matching uses kudurru's most-specific IP/CIDR tier. Review overlapping
assignments, inherited defaults and the license required for additional policies.
Filter parsing and exception scope can differ between products: destination
block-list exceptions are local to their object; explicit allow lists override
blocks across selected objects. Do not assume every downloaded source rule is supported.

## Select, apply and inspect the tracked result

Inspect each object's `kind` and `metadata`, `settings`, and every warning in
the preview. Repeat the command with exactly the source and conversion options
you reviewed, adding selections and `--apply --wait`:

```sh
kudurru import adguard --file AdGuardHome.yaml -n default \
  --select-object Policy/default/default --select-setting upstreams --apply --wait

kudurru import blocky --file config.yaml -n default --skip-settings --apply --wait
```

All items are selected by default. `--select-object Kind/[namespace/]name` and
`--select-setting SECTION` select only the named items; repeat either flag to
add items. `--skip-settings` selects all objects unless explicit object selections
are given. Unknown, duplicate and empty selections fail before destination
writes. Dependencies are not selected automatically: include referenced lists,
client groups, zones and upstream settings, or ensure they already exist.

`--apply` starts the same tracked background migration as API imports, using
normal validation, ownership, permissions and licensing. Without `--wait`, exit
status zero means the server accepted the job. With `--wait`, unsuccessful or
unconfirmed final reports return an error. Canceling the wait leaves the
accepted job running; its run ID remains in the diagnostic output. Inspect it
with `kudurru import runs RUN_ID`.

Successful objects remain if another item fails. Settings apply only after every
selected object succeeds. The report distinguishes skipped items, object errors,
settings validation/ownership errors and settings not applied after object
failure. The journal contains source type, planned identities and outcomes;
it excludes source records, credentials and offline filenames.

Authority options are shared with the live importers: explicit `--nameserver`
or `--nameserver-address` requires no discovery. `--use-vips` explicitly reads
the destination API; `--update-nameservers` derives targets from the selected
destination context. For example, add `--local-zone home.arpa` when reviewing
AdGuard exact local records. Review the changed authority before applying, then
follow the [cutover and rollback checklist](/migrate/cutover/).

The converters were derived from public format documentation:
[AdGuard Home Configuration](https://github.com/AdguardTeam/AdGuardHome/wiki/Configuration)
and [Blocky configuration](https://0xerr0r.github.io/blocky/latest/configuration/).
