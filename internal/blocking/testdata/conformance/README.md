# Blocking conformance corpus

Each `*.txtar` file is one scenario run by `TestConformance`
(`internal/blocking/conformance_test.go`). The cases are written for
kudurru by hand (Apache-2.0); no rules were copied from GPL-licensed lists
(hagezi, AdGuard filters) or from AdGuard Home / urlfilter.

## Format

Lines before the first section are free-form comments. Sections:

- `-- list <name> <block|allow> <format> [hostsSubdomains] --` followed by the
  raw list body, exactly as a list source would serve it.
- `-- options --` with `allowOnly` and/or `scope=selection`.
- `-- counts <list> --` with `key=value` pairs checked against the compiled
  list: `rules`, `exceptions`, `unsupported`, `invalid`, `ignored`,
  `entries`. Keys that are not given are not checked.
- `-- expect --` with one query per line:
  `<name> <block|allow|pass> [<list> <rule>]`. `-` stands for an empty list
  or rule (allow-only blocks). A `cname:` prefix runs the name through the
  response-side check (`Selection.EvaluateResponse`), an `ip:` prefix checks
  an answer address. Text after ` #` is a comment.

All lists of a file form one selection in file order (allow-mode lists are
consulted first, as `Snapshot.Select` does).

`{x*N}` anywhere in a list or expect line expands to N copies of `x`, so
long names stay readable: `{a*63}.{b*63}.{c*63}.{d*61}` is exactly 253
bytes.

## Relation to AdGuard Home

The architecture (§7.3) calls for cross-checking this corpus black-box
against an AdGuard Home container. That needs Docker and is a follow-up;
the cases where kudurru deliberately differs from AdGuard Home are marked
`deviation` in the comments:

- `*.x` and `||*.x^` also match `x` itself (blocky's semantics, used by the
  `*.domain` wildcard list formats).
- `@@` exceptions cancel deny rules of their own list only, unless the
  selection uses `scope=selection` (AdGuard Home merges every list into one
  engine).
- `||x` without `^` is treated as `||x^`.
- Rules with modifiers are not applied in v0.1 (counted as unsupported).
