# Manage a zone and its records

**Before you start**

- You need permission to read the zone.
- You need the `admin`, `operator` or scoped `zone-admin` role to edit its records.
- Use [Create a local DNS name](../local-dns/create-name.md) if you need a new primary zone.

## Find and edit a record

1. Open **DNS** > **Zones**.
2. Click the zone name.
3. Enter a name, type or value in **Filter records**.
4. Click **Edit** in the record's row.
5. Change the target or TTL.
6. Save the record.

![A zone detail page with its owner, readiness, record table and import and export actions.](../images/zone-detail.png)

*Figure 1. Record edits belong to the primary zone that stores them.*

Use **Add record** to create another name or type.
Use **Add target** in the editor for another value in the same record.
**Disabled** keeps the record but removes it from DNS answers.
Secondary, forward and stub zone records come from elsewhere and are read-only.
For a managed zone, edit its file or Kubernetes resource.

## Import or export a zone

1. Click **Export** to inspect the current BIND zone file.
2. Click **Download** to keep a copy before changes.
3. Close the export dialog.
4. Click **Import BIND**.
5. Select a **Zone file** or paste the zone text.
6. Click **Import**.
7. Read the result and any warnings.

**Import zone file** on **Zones** creates a zone from a BIND file.
**Send NOTIFY** tells configured secondary servers to fetch updates and needs transfer permission.
Use the [forward zone procedure](../admin/07-zones-and-upstreams.md#create-a-forward-zone)
when a domain needs a particular upstream instead of local records.

## Check the result

Query the edited name from a client that uses kudurru.
Confirm its value and authoritative source in [Query log](query-log.md).
Inspect the record table after an import before you depend on the new answers.
