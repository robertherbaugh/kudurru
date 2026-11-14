### Added

- Client-group record views (split horizon): an endpoint with `kudurru: { view: <ClientGroup> }` is now served only to clients of that ClientGroup (in the record set's namespace), replacing the default answer of the same name and type for them. Other clients keep the default answers, and view-only names do not exist for them. Works on the SQLite and Kubernetes drivers.

### Changed

- Zone transfers, zone exports, RFC 2136 UPDATE and integration sinks (Cloudflare, UniFi) carry the default data only; client-group view records never leave the server through them. Cloudflare no longer selects endpoints that belong to a client-group view.
- Any client-group view makes the dnsdist edge bypass both its authoritative and its resolver packet cache (`zone-client-view`), because the edge cache cannot tell clients' groups apart.
- The `DNSRecordSet` schema now requires `kudurru.view` to be `public` or a valid ClientGroup name.
- Views are not served in DNSSEC-signed zones, cannot hold NS, DNAME, SOA or DNSSEC records, and cannot sit at or below a delegation or DNAME (including, on SQLite, DNSSEC data or a delegation added by RFC 2136 UPDATE). A zone serves at most 16 views; the budget is shared by every record set of the zone, first come first served. On Kubernetes such endpoints are reported on their record set; on SQLite only when nothing else in the record set is served.
- A client-group view may replace only default data of its own namespace: a view endpoint that would override another namespace's name and type is refused as `Duplicated`, as the same default endpoint would be. Views of one name in two namespaces refer to two different ClientGroups and do not conflict.
- A view name (and the empty non-terminals it creates) suppresses a default wildcard for that group's clients, as any existing name does.

### Fixed

- Endpoints whose `kudurru.view` named a ClientGroup were silently withheld from every client.
