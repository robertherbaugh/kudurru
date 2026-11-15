### Changed

- Integrations: a check, preview, sync or action reviewed just before the integration host wrote its own status now runs instead of answering `409` once the host has read that write back (within one capture interval, two seconds by default; until then a reread and one retry may still be needed), and `GET /integrations/{namespace}/{name}` keeps showing the status of a healthy running instance after such a write (`statusKnown` stays true). A change of the spec, labels, annotations or finalizers still makes the review stale.
- Integrations: `GET /integrations/manifests` gains `fieldLimits`, one entry per spec field this node's host refuses (with the status reason and message) or accepts without effect (with the field it needs). On Kubernetes the refused UniFi record mirror field is now `spec.unifi.push.mirrorRecords.enabled`, and `spec.unifi.push.dnsServers`, `networks` and the record mirror's `types`, `domains`, `labels` and `exclude` are listed as inert.
- Integrations: the UniFi `dnsServers` schema now gives the canonical dotted IPv4 pattern the provider accepts and names the addresses it refuses.
- Integrations: the audit row of a UniFi `block-client` or `unblock-client` action names the client in lowercase (`for client 02:ab:cd:00:00:01`), whatever letter case was sent.

### Fixed

- Integrations: an instance whose provider setup failed (for example a provider that was briefly unreachable, or a setup cut short while a node was starting) is set up again with backoff (from one poll interval up to five minutes) instead of staying `ProvisionFailed` until its object changed. A setup whose peer offers no role the instance can take is not retried.
- Integrations on Kubernetes: a `deletionPolicy: Delete` UniFi Integration that gained `spec.unifi.push.dhcpDNSServers` or `clientActions` is now cleaned up when deleted instead of staying Terminating until released; a refused push gets its own condition message.
- Integrations: a refused Integration document no longer repeats a pasted secret used as a map key under `spec.credentials`; known credential names are still named.
- External zones: on the SQLite store the `SourcesNotRead` message no longer points at `externalZones.enabled`, which that store refuses, and a zone already Degraded for a more specific reason (for example `Duplicated`) keeps that reason.
