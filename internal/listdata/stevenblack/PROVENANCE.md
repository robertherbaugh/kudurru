# Provenance: embedded starter blocklist

SPDX-License-Identifier: MIT

| Field | Value |
|---|---|
| File | `hosts` (this directory), embedded into the kudurru binary by `internal/listdata` (`go:embed`) |
| Purpose | the first-boot starter `BlockList` "starter" (dev-docs/design-record.md §6.4), so blocking works offline |
| Upstream repository | https://github.com/StevenBlack/hosts |
| Upstream path | `data/StevenBlack/hosts` ("Steven Black's ad-hoc list") |
| Upstream commit | `2025dea10a1d17c41acf1035056e5c4a2913ebfc` (master, 2026-10-07) |
| Fetched | 2026-10-11 from `https://raw.githubusercontent.com/StevenBlack/hosts/2025dea10a1d17c41acf1035056e5c4a2913ebfc/data/StevenBlack/hosts` |
| SHA-256 | `acd9c89e74c48c625034651f2766e79f4ea835a38f6bf604032ea5a9caf416c1` |
| Size | 96,808 bytes, 3,277 lines, 2,854 host entries |
| Licence | MIT, Copyright (c) 2023 Steven Black (`LICENSE` in this directory, copied from the same commit; SHA-256 `8e52717212b5051232dd31fb2247310bd91d5295eaab380dbea6454d0268443b`) |
| Licence evidence | the repository licence (`license.txt`, GitHub SPDX `MIT`) and the source's own `data/StevenBlack/update.json`, which declares `"license": "MIT"` |
| Modifications to the file | none; the bytes are identical to `internal/blocking/testdata/corpus/stevenblack/hosts` |
| Transformation at runtime | `listdata.StarterBlockList` keeps the host names of the file's rules (comments, blank lines and the `0.0.0.0` prefix dropped, duplicates removed) as the inline `domains` source "stevenblack-adhoc" of the BlockList, and records this provenance in the `kudurru.io/provenance` annotation |

Only this one source of the StevenBlack project is used. The project's
unified `hosts` file aggregates third-party sources under other licences
(some CC BY-SA, some non-commercial), so it is never embedded; this file is
the part Steven Black maintains himself under MIT.

Copyleft lists (hagezi, GPL-3.0; AdGuard filters, GPL-3.0) may be
referenced by URL at runtime like any other data but are never embedded
(dev-docs/design-record.md §6.4, §14 risk 16). `TestProvenance` in
`internal/listdata` fails the build when this file stops naming a
permissive licence, when the licence text is missing, or when the embedded
file's SHA-256 no longer matches the one recorded above.
