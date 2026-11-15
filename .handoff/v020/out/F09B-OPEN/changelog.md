### Changed

- Kubernetes: while the controller reports a DNSRecordSet that a published primary zone admits as `Duplicated`, `Occluded` or `Invalid`, every Cloudflare Integration now shows `Ready=False` with reason `SourceConflicted` and a message naming the record sets, instead of silently stopping. UniFi inventory and groups keep running. Publication resumes on its own once the record sets are fixed or removed.

### Fixed

- Kubernetes: a refused Integration whose status write keeps failing (for example because of RBAC or an admission webhook) no longer retries in a tight loop; it retries after one poll interval, doubling up to five minutes.
- Kubernetes: an Integration that kudurru refuses (for example one outside the release namespace) no longer has the allowlisted credential read or its fingerprint written to its status.
- Kubernetes: a record set conflict reported after a Cloudflare sync started now stops that sync's next provider request, not only the next sync.
- Integrations: Cloudflare Integrations that reserve the shared request budget at the same moment (for example several of them starting on two Kubernetes replicas) no longer leave one of them inert until its object changes; a reservation that loses the race is attempted again, after a growing pause, until the request's own timeout.
