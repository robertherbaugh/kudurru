// Package domainlist keeps the dashboard's Allowlist and Denylist: single
// domains, wildcards and regular expressions a user allowed or denied, each
// with its own switch, comment, scope and provenance (Pi-hole's Domains
// page, Technitium's Allowed and Blocked zones).
//
// The entries live on BlockLists, as structured entries of the source "ui"
// (spec.sources[].entries): ui-allow and ui-deny hold the entries for
// everyone, ui-allow-<group> and ui-deny-<group> those scoped to one
// ClientGroup (in the group's namespace). Group lists also record explicit
// spec.clientGroupRefs; policy selection enforces that original winning
// membership independently of attachment, including shared policies and legacy
// group-list names. An everyone list is referenced
// from every policy of its namespace kudurru may edit, a group list from
// the policies whose clientGroupRefs name the group (allowListRefs for
// allow, blockListRefs for deny), as unblock approvals did before; a
// policy managed by a file or an integration is reported instead of
// edited. Every write attaches its lists, and the leader's Reconcile keeps
// the references current as policies come and go or change their groups.
// Allow wins over block, so an allow entry overrides every block list of
// the policies concerned.
//
// One logical entry is (namespace, action, type, value) across those
// lists: an entry scoped to two groups is a copy in each group's list,
// edited together. Types: exact ("ads.example.com"), wildcard
// ("*.example.com": the name and every name below it, like
// ||example.com^) and regex (RE2). A list that ends up empty is deleted
// and its references removed.
//
// Lists written before structured entries kept their domains as inline
// lines ("example.com", "||example.com^", "/re/"). Load reads both forms;
// every write converts a list's lines into entries, and Migrate (run once
// by the leader) converts every list kudurru may edit, taking the
// provenance of lines an unblock approval still holds from its
// status.allow. Lines that are none of these rules stay inline, so the
// migration loses nothing.
package domainlist
