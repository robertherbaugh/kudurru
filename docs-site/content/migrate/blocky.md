---
title: Move from Blocky to kudurru
linkTitle: From Blocky
description: Preview the supported subset of Blocky YAML, review differences in filtering and local mappings, and apply a tracked migration.
weight: 22
---

In v0.2, preview a saved groups-based Blocky configuration with:

```sh
kudurru import blocky --file config.yaml -n default -o json > preview.json
```

Review the objects, settings and loss report using the
[offline configuration guide](/migrate/offline-config/). Conversion reads no
referenced files and fetches no URLs. It supports common upstream groups,
conditional routes, hosts/domain/wildcard/RE2 lists, IP/CIDR client policies,
exclusive allow mode, and simple suffix-wide IP mappings. Keep Blocky serving
DNS and retain its configuration and list files during review.

Blocky can use client-name lookups, dynamic list loading, schedules, automatic
reverse records, query rewrites and other behavior that the converter does not
reproduce. These losses are reported; a completed migration job does not prove
identical DNS behavior. List groups with skipped sources or schedules are not
attached to filtering policies. Rebuild those policies before applying.

`customDNS.mapping` covers the mapped domain and its descendants. Converted
IP mappings preserve that scope with apex and wildcard records in an
authoritative zone, adding SOA/NS answers and omitting automatic reverse
records. They require unmapped-type filtering and no query rewrite. Local
answers bypass filtering in kudurru. Test these differences explicitly.

Apply only reviewed selections with `--apply --wait`, inspect the report with
`kudurru import runs RUN_ID`, and follow the
[cutover and rollback checklist](/migrate/cutover/). User accounts, listener
privileges, credentials, certificates, runtime data and local source paths need
separate configuration on the destination.
