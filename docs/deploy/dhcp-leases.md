# DHCP leases, backups and imports

[DHCP guides](dhcp.md) · [Step-by-step setup](../guide/dhcp/README.md)

## Lease durability and backups

The default scope uses `dhcp.db`; named scopes use `dhcp-<name>.db`, under
`node.dataDir`, separately from configuration objects in `state.db`. Each
allocation commits before its OFFER or ACK. WAL/full synchronization and
exclusive database and directory ownership protect restart recovery.
Preserve these files across restarts. API-managed options and reservations
also live in the scope databases. Dashboard desired setup and apply plans live
in `dhcp-settings.json`, protected by `dhcp-settings.lock` for the process
lifetime even when DHCP is disabled. Saves use atomic replacement and file
and directory synchronization; they do not rewrite YAML or mutate live lease
databases.

Listing and export read committed cached state and do not rebuild DNS snapshots.
They still copy/sort the live set under the store lock. Renewal updates affected
rows only; durable SQLite writes remain mandatory. Comparative 1,000/10,000-lease
[benchmarks](../architecture/dhcp.md#persistent-state-and-transfer) show the
improvement, but do not establish production throughput. Validate your expected
lease count, storage and request rates before deployment.

Lease schema version 4 retains the applied setup token and adds persistent DNS
claim ranks and replica progress. At startup, a new
token must match its expected database revision and preserve live ownership;
the settings, revision and token commit together. Matching tokens retain later
live edits instead of reapplying saved options. Earlier lease schemas migrate
transactionally. The JSON setup document has its own version; schema v4 refers
to the lease database, not the JSON export or setup format.

Shortening `leaseDuration` never shortens retention of an existing binding:
a renewal can advertise the new duration while the database holds the address
through its earlier expiry, in case the client missed the ACK. Configuration
changes are checked against live state before committing. Removing or renaming
a scope retains its database and can block the next DHCP startup while that
retired database has live allocations. A renamed scope uses a different
database; allocations are not moved to its new name. Keep the original name
until leases, offers and quarantines drain. To recover a failed rename/removal,
restore the original scope in saved setup using the observed revision from
its retained database, then restart. Removing the last scope stages disabling
DHCP; it still does not delete lease state.

The ordinary `kudurru backup` exports configuration objects, not these lease
databases or `dhcp-settings.json`. Native lease exports cover allocations only,
not saved setup, managed options or reservations. Stop the node before taking
or restoring a filesystem backup of the data directory, including the setup
registry, `.dhcp-dns-claims.sqlite`, SQLite WAL files and, for quorum nodes,
the entire `dhcp-raft` directory. The claim registry establishes cross-scope
name order and must stay with its lease databases. Do not delete a scope database or restore an
old copy while clients hold leases absent from it.

## Lease import and export

The dashboard exports native version 1 JSON preserving scope, subnet,
client identity, address, hostname, absolute expiry and offered/bound/declined
state. When using the API directly, save the `document` field, not its response
wrapper. Native imports require a matching scope and subnet. Client IDs are
opaque bytes encoded as base64 with an explicit identity kind; do not rewrite
them as MAC addresses.

The import dialog accepts native JSON or a dnsmasq **lease file**, pasted as
text, up to 4 MiB. The API takes `format: native|dnsmasq` and `data` containing
the file text. An import merges all valid records in one transaction, reporting
imported, unchanged and expired/skipped counts. It cannot replace live
ownership, shorten expiration or conflict with reservations. Any invalid row
rejects the entire import. Unsupported infinite leases are rejected. Import
works in file management mode because it changes runtime allocations only.

dnsmasq hostname fields follow the same normalization as option 12: trailing
NULs are stripped and unusable names are omitted. Native JSON stays strict and
lossless, rejecting invalid hostnames instead of normalizing them. Other
invalid records still reject the entire import in either format.

The public export uses native JSON; it preserves holds and quarantine that a
dnsmasq lease file cannot represent. Import does not configure network options
or reservations. Save and recreate those separately before cutover.
