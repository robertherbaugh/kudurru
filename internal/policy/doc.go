// Package policy turns ClientGroups, Policies and BlockLists into an
// immutable Index that answers, per query and without allocating, "which
// groups is this client in, and which policy applies to it right now?"
// (docs/architecture.md §7.1 steps 3 and 7, §7.2, §4.1).
//
// # Client identification
//
// Identify walks fixed tiers and stops at the first tier with a match:
//
//  1. exact IP (ClientMatcher.ip)
//  2. MAC (ClientMatcher.mac) against pipeline.Client.MACs; nothing fills
//     MACs before the v0.2 inventory integrations, so this is the slot
//  3. longest-prefix CIDR (ClientMatcher.cidr), IPv4 and IPv6 separately
//  4. hostname (ClientMatcher.hostname, exact, case-insensitive), then
//     nameGlob (path.Match syntax; a hostname containing *, ? or [ is a
//     glob too, as in the "ps5*" example of §4.1), against the names a
//     HostnameResolver knows for the address; without a resolver (v0.1)
//     this tier never matches and the Index says so in a Diagnosis
//  5. clientID (DoT SNI / DoH path identifiers, ClientMatcher.clientID)
//  6. catch-all CIDRs 0.0.0.0/0 and ::/0. They rank below every specific
//     matcher, so the bootstrap ClientGroup "default" (which lists both)
//     never hides a hostname or clientID match
//  7. the default group: ClientGroup "default" in Options.DefaultNamespace,
//     which §7.2 defines as matching everything
//
// Within a tier every matching group is joined: an IP or CIDR listed by
// two groups puts the client in both, and the Index reports the tie as a
// Diagnosis (ReasonTie) because it is almost always a configuration
// mistake. The result is a *Membership whose GroupIDs ("namespace/name")
// are precomputed and shared.
//
// # Policy selection
//
// The candidate policies are those whose clientGroupRefs name one of the
// client's groups (in the policy's own namespace). Policies with a
// schedule are filtered at query time: weekdays (mon..sun), start and end
// ("HH:MM") in an IANA time zone; end before start is a window that
// crosses midnight and belongs to the weekday it starts on, start equal to
// end is 24 hours. When no candidate is active, Policy "default" in
// Options.DefaultNamespace (the bootstrap policy, priority 0) applies as
// the fallback; when that is absent or itself out of schedule, no policy
// applies and nothing is filtered.
//
// The active policies are merged into one Decision: the lists of all of
// them (allow lists before block lists, so an allow match in any group's
// policy wins; blocked services after the block lists), and per setting
// (blockMode, blockTTL, upstreamGroup, queryLog, safeSearch,
// safeSearchMode) the value of the highest-priority policy that sets it; allowOnly comes from the highest-priority policy. Equal priorities
// are ordered by namespace/name. Decisions, including the
// blocking.Selection resolved against the compiled lists of the same
// snapshot, are precomputed for every combination of the scheduled
// policies of a group set (eagerly up to 64 combinations, lazily beyond),
// so SelectPolicy costs a map lookup or two plus one time-zone conversion
// per scheduled policy.
//
// # Lists
//
// A Policy names BlockLists by name in its own namespace. The compiled
// blocking.Snapshot is searched for "namespace/name" first and then for
// the bare name, so either naming convention of the list compiler works.
// A referenced BlockList that exists but is not compiled yet is reported
// by NotLoaded, the readiness input of blocking.allowDegraded.
//
// blockedServices IDs resolve to the catalogue's compiled block lists
// "service:<id>" (internal/listdata/catalogue), which the list resolver
// compiles into the same snapshot. blockedCategories add every service
// the catalogue in service has in those categories, minus the policy's
// allowedServices (EffectiveServices); the Index is rebuilt with every
// snapshot, and a catalogue change recompiles the snapshot, so a service
// a newer catalogue adds to a blocked category is blocked from then on.
// An ID the catalogue in service does not have (Options.Services) is
// skipped with Diagnosis UnknownService (UnknownCategory for a
// category); one that is known but not compiled yet counts as NotLoaded.
//
// # Serving
//
// Builder is the zonecache.Builder (view "policy") that rebuilds the Index
// for every snapshot; Source reads it per query. The identify stage stores
// the Decision in pipeline.Request.Policy, and FromRequest reads it back.
package policy
