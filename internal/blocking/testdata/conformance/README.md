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
- `-- context --` with `dnstype=AAAA`, `clientIP=192.0.2.5`, `clientID=fixture`,
  `tags=device_pc|user_child`, and/or `names=FixturePC`. Defaults are IN/A
  without client identity selectors.
- `-- counts <list> --` with `key=value` pairs checked against the compiled
  list: `rules`, `exceptions`, `unsupported`, `invalid`, `ignored`,
  `entries`. Keys that are not given are not checked.
- `-- expect --` with one query per line:
  `<name> <block|allow|pass|rewrite> [<list> <rule>]`. `-` stands for an empty list
  or rule (allow-only blocks). A `cname:` prefix runs the name through the
  response-side check (`Selection.EvaluateResponseContext`), an `ip:` prefix checks
  an answer address. Text after ` #` is a comment.

All lists of a file form one selection in file order (allow-mode lists are
consulted first, as `Snapshot.Select` does).

`{x*N}` anywhere in a list or expect line expands to N copies of `x`, so
long names stay readable: `{a*63}.{b*63}.{c*63}.{d*61}` is exactly 253
bytes.

## Relation to AdGuard Home

`TestAdGuardBlackBox` runs a separate official AdGuard Home v0.107.79
executable against a first-party local DNS upstream. It compares 45 cases
covering every supported modifier, exceptions and priority, CNAME/address
answer inspection, typed rewriting, client names/tags, and malformed/unknown
rules. It uses no AdGuard source code or filter-list data. Docker is optional;
the external executable is sufficient:

```sh
KUDURRU_ADGUARD_BINARY=/absolute/path/AdGuardHome \
  go test ./internal/blocking -run '^TestAdGuardBlackBox$' -v
```

Without the environment variable the opt-in test skips explicitly. The
ordinary corpus and modifier/stage unit tests always run. Comparisons ignore
record ordering and TTLs while checking response codes and record contents.

Intentional product semantics:

- `*.x` and `||*.x^` also match `x` itself (blocky's semantics, used by the
  `*.domain` wildcard list formats).
- `@@` exceptions cancel deny rules of their own list only, unless the
  selection uses `scope=selection` (AdGuard Home merges every list into one
  engine).
- `||x` without `^` is treated as `||x^`.
- Explicit allow-mode lists form a separate engine; block-mode badfilter
  directives cannot cancel their authorization. ClientID selectors extend
  the public client-name/address grammar.
- Badfilter identities normalize simple domains and modifier ordering,
  matching the compiler's fast-rule rendering.
- Answer inspection retains the original query type and pinned client
  context. The public syntax documents a response-record type change in
  AdGuard v0.108; this release preserves kudurru's query context contract.

Two v0.107.79 differences are asserted explicitly in the black-box test:

- Bare `NOERROR` loses to a simultaneous address rewrite in that version;
  kudurru follows the public documented keyword-first priority.
- A combined `$client`/`$dnstype=AAAA` rule applies to a direct target question
  but is skipped on a returned CNAME in that version. Kudurru applies the
  pinned selectors during final answer inspection.

The executable comparisons therefore contain 43 matching cases and two
documented differences, not a claim of byte-for-byte equivalence.
Geolocation (`respgeo`), CIDR answer rules and hosts-file rewrite directives
remain explicitly unsupported. Unknown modifiers reject the entire rule.
