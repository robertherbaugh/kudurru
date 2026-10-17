// Package zone is kudurru's authoritative data model (dev-docs/design-record.md
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
//   - Transfers out (xfr.go): Zone.AXFR streams the zone in canonical
//     order with the SOA first and last in chunks of at most 64 KiB, and
//     Zone.TransferAllowed applies zoneTransfer, transferACL and
//     transferTSIGKeyRefs. NOTIFY out (notify.go): NotifyMessage and
//     SendNotify (RFC 1996), optionally TSIG-signed.
//   - Set.Gate is the D80 gate the authoritative stage applies: open,
//     unsynced (every query SERVFAIL) or stale (SERVFAIL for primary zones
//     only).
//   - ServedRecordSets (select.go) decides which DNSRecordSets a zone is
//     built from; the zone cache and the controller both use it, so
//     status.contentHash is the hash of the zone every replica serves.
//
// miekg/dns types appear in this package's API (RRset, Result) on purpose:
// zone is one of the packages §2 allows to use them, and RRset is the
// boundary type the rest of kudurru handles.
package zone
