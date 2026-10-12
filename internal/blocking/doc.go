// Package blocking is kudurru's blocking engine core (docs/architecture.md
// §7.1 step 7, §7.2, §7.3): it compiles BlockList sources into immutable
// matcher chains and answers "is this name blocked for this client, and by
// which rule?" without allocating.
//
// # Pieces
//
//   - parser: the clean-room list parser (hosts, domains, adblock, regex,
//     auto-detection, modifiers kept for v0.2).
//   - matcher: the exact, suffix and regex matchers plus an IP set for
//     response-side checks.
//   - List: one compiled BlockList: a primary chain (deny entries for a
//     block list, allow entries for an allow list) and, for block lists, an
//     exception chain built from the list's own @@ rules. Compile skips the
//     work and returns the previous List when the CRC-32 of the normalised
//     rule lines of every source is unchanged.
//   - Snapshot: an immutable set of Lists. The owner (the blocking stage and
//     the list refresher) publishes it through an atomic.Pointer[Snapshot]
//     and replaces it copy-on-write with Rebuild; readers never lock.
//   - Selection: the lists one policy applies, with Evaluate for the
//     question and EvaluateResponse for CNAME targets and answer addresses.
//
// # Decision order (§7.1 step 7)
//
//  1. A match in any allow-mode list of the selection allows the query
//     ("allow wins across lists").
//  2. Otherwise, in allow-only mode, the query is blocked.
//  3. Otherwise each block-mode list is checked in selection order: a deny
//     match blocks unless the same list has a matching @@ exception
//     (ScopeList, the default; ScopeSelection lets any list's @@ cancel any
//     list's deny, as AdGuard Home's merged engine does).
//  4. After the upstream answer, EvaluateResponse applies steps 1 and 3 to
//     every CNAME target and answer address; allow-only does not apply
//     there because the question itself was allowed.
//
// $important (deny beats @@) is part of the v0.2 modifier slow path; v0.1
// counts such rules as unsupported.
//
// All names are normalised with NormalizeName: lower-case ASCII without the
// trailing dot. List entries are normalised the same way by the parser,
// including IDNA conversion of Unicode entries to A-labels, which is the
// form query names arrive in.
package blocking
