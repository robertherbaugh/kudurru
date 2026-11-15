### Changed

- Removing every `domain` of a webhook Integration ends its record publication: the record sets it published stay at the peer (as under `Retain`), and a webhook whose peer offers only records is refused as `ProvisionFailed`. Deleting it with `deletionPolicy: Delete` still removes those record sets.
- A desired record at a name another Kudurru's TXT registry row still claims, without a data record, is now reported as a conflict instead of being refused on every sync. Such a claim holds the whole name: no record set of the Integration there is updated or deleted, and a `deletionPolicy: Delete` cleanup stops with `RecordsConflict` until the stale `_kudurru-<type>.<name>` TXT row is removed at the peer.
- Hook events: `zone.changed` is sent once per committed serial of a zone (no extra event when a zone is created), and a resync event is always delivered, carrying the zone's committed serial.
- Hook delivery records are now removed by every running Integration, not only hook instances, an hour after their 24-hour deadline (Integrations without hooks check at least every 15 minutes); an Integration whose peer stopped advertising hooks drops its own undelivered events at their deadline and reports `EventDeadLettered`.
- Older versions of an event subject are removed before the newest one when a hook instance trims its delivery records, so a status-only write resends an event under its old `X-Kudurru-Delivery` ID far less often.

### Fixed

- A webhook Integration with `deletionPolicy: Delete` whose domains had all been removed could never finish its cleanup.
- A Cloudflare delete of a record set that was already gone could not be confirmed in the same run.
- With a small change-feed retention, hook instances no longer fall back to a resync on every feed compaction.
