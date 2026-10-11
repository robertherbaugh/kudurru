// Package zone is kudurru's authoritative data model (docs/architecture.md
// §3.3, §4.1, §4.2): DNSZone and DNSRecordSet objects in, immutable
// per-zone trees out.
//
// The flow is:
//
//   - NewBuilder validates a DNSZone and Builder.Add turns each
//     api/v1alpha1 Endpoint into RRsets, parsing presentation-format
//     targets with miekg/dns against the zone origin and default TTL.
//     Disabled, expired and view-scoped endpoints are skipped (D28);
//     anything that does not parse or conflicts (CNAME exclusivity,
//     duplicates across objects) is reported as a field.Error whose path
//     points into the offending object, ready for a condition.
//   - Builder.Build synthesises the SOA (serial from status.serial, D12)
//     and the apex NS RRset from the DNSZone spec, normalises every RRset
//     and computes the canonical ContentHash. The leader compares it with
//     status.contentHash and calls StampSerial (max(prev+1, unix_now)).
//   - Zone.Lookup answers one question from the tree: Success, NoData,
//     NXDomain, Delegation, or CNAME when a CNAME/DNAME chain leaves the
//     zone, with RFC 4592 wildcards, RFC 6672 DNAME and a shared chain
//     budget of MaxChain hops.
//   - Snapshot is an immutable set of zones with longest-suffix routing
//     (Match; the root zone "." is allowed). The zone cache publishes one
//     through a Holder (an atomic.Pointer) and the authoritative stage
//     reads it without locks.
//   - ImportBIND and ExportBIND convert between RFC 1035 master files and
//     the object model; Journal and Diff are the IXFR seam (v0.2).
//
// miekg/dns types appear in this package's API (RRset, Result) on purpose:
// zone is one of the packages §2 allows to use them, and RRset is the
// boundary type the rest of kudurru handles.
package zone
