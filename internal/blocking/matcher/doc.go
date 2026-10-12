// Package matcher holds the compiled matchers of a block list
// (docs/architecture.md §7.3), cheapest first:
//
//  1. Exact: lower-cased names bucketed by length, each bucket one sorted
//     concatenated string searched with a binary search; a maphash bit
//     filter in front answers most misses with one memory access, and a
//     sampled fence index (8 bytes per 16 records) keeps the binary search
//     of a hit inside one block.
//  2. Suffix: "name or any subdomain of name" entries (||x^, *.x and
//     hosts-style entries with subdomain semantics), bucketed by TLD with
//     the same buckets and a bit filter over the full base names; lookups
//     walk parent labels broadest-first.
//  3. Regex: RE2 expressions (capped per list) indexed by a four-byte gram
//     of their required literal, so a lookup runs only the expressions
//     whose gram occurs in the name plus the few without a usable literal.
//
// IPSet adds the response-side IP entries (bare IP addresses in a list).
//
// The length-bucketed sorted-string layout and the cheapest-first chain are
// adapted from blocky v0.35.0 (github.com/0xERR0R/blocky, Apache-2.0,
// cache/stringcache); see NOTICE. The suffix layout follows the design in
// docs/architecture.md §7.3 (TLD buckets plus a bit filter) rather than
// blocky's map-per-node trie, which costs far more memory per entry.
//
// All matchers are immutable once built and safe for concurrent use; a nil
// matcher is empty. Lookups never allocate. Callers pass names that are
// already normalised: lower-case ASCII, no trailing dot (see
// blocking.NormalizeName). Builders are not safe for concurrent use.
//
// Every entry carries an opaque one-byte tag that the caller uses to
// remember the syntax the entry was written in, so the matched rule can be
// rendered back for the query log without storing the original line.
package matcher
