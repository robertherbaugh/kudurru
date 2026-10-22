// Package blocking is kudurru's blocking engine core (dev-docs/design-record.md
// §7.1 step 7, §7.2, §7.3): it compiles BlockList sources into immutable
// matcher chains and answers "is this name blocked for this client, and by
// which rule?" The ordinary fast lookup path does not allocate.
//
// # Pieces
//
//   - parser: the clean-room list parser (hosts, domains, adblock, regex,
//     auto-detection and a complete modifier candidate stream).
//   - matcher: exact, suffix and regex matchers, an IP set, and an uncapped
//     required-literal index for contextual DNS patterns.
//   - List: one compiled BlockList: a primary chain (deny entries for a
//     block list, allow entries for an allow list) and, for block lists, an
//     exception chain plus compiled modifier rules. Compile returns the
//     previous List when the CRC-32 of normalized rule lines and all options
//     are unchanged. Badfilter directives are collected before compilation.
//   - Snapshot: an immutable set of Lists. The owner (the blocking stage and
//     the list refresher) publishes it through an atomic.Pointer[Snapshot]
//     and replaces it copy-on-write with Rebuild; readers never lock.
//   - Selection: the lists one policy applies, with EvaluateContext for
//     the question and EvaluateResponseContext for returned targets and
//     addresses. Context pins the query type and operator inventory identity.
//
// # Decision order (§7.1 step 7)
//
//  1. A match in any allow-mode list of the selection allows the query
//     ("allow wins across lists").
//  2. Otherwise, in allow-only mode, the query is blocked.
//  3. Explicit dnsrewrite rules produce immutable typed synthetic plans before
//     ordinary denies. Rewrite-specific exceptions cancel those plans.
//  4. Otherwise important exceptions beat important denies, which beat ordinary
//     exceptions and denies. ScopeList is the default: an exception cancels
//     only its own list. ScopeSelection merges selected block-mode lists,
//     including badfilter cancellations. Explicit allow lists remain separate.
//  5. After raw cache lookup and configured synthesis, response inspection applies
//     the same identity and query selectors to CNAME/SVCB targets and addresses.
//     An explicit allow-list question match skips these checks.
//
// Supported modifiers are dnstype, dnsrewrite, client, ctag, important,
// badfilter and denyallow. Unknown or invalid modifiers reject the entire
// rule. Geolocation and hosts-file rewrite directives remain diagnosed as
// unsupported; they never become unconditional denies.
//
// All names are normalized with NormalizeName: lower-case ASCII without the
// trailing dot. List entries are normalized the same way by the parser,
// including IDNA conversion of Unicode entries to A-labels, which is the
// form query names arrive in.
package blocking
