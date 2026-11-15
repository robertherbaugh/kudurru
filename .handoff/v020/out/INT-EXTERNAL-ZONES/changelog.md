### Added

- Kubernetes external zones: a DNSZone of type `external` is filled from Ingresses, Gateway API HTTPRoutes, GRPCRoutes and TLSRoutes (through Gateways that accepted the route) and LoadBalancer Services, as its `spec.external` selects. Turn it on with the host setting `externalZones.enabled` (Helm value `externalZones.enabled`). The elected replica keeps one source-owned DNSRecordSet per source and zone, honors the external-dns `hostname`, `target` and `ttl` annotations (and their `alpha` forms), removes records when their source goes away and never changes the source objects. No license key is needed.
- An external zone on a server without `externalZones.enabled` is reported `Degraded` with reason `SourcesNotRead`, so an empty zone is no longer silently `Ready`.
- Helm: `externalZones.enabled` grants the ClusterRole read access to `ingresses` and the Gateway API `gateways`, `httproutes`, `grpcroutes` and `tlsroutes` only when it is set.

### Changed

- An external zone serves only the records derived from its Kubernetes sources: the API refuses a DNSRecordSet for an external zone with `conflict`, and a record set written another way (kubectl) is reported `NotSelected` and not served. RFC 2136 UPDATE into an external zone is refused with `NOTAUTH`. The SQLite store still refuses the `external` type. Each source kind is read as a complete list of at most 4,096 objects and 32 MiB (every Service counts); a kind over the bound keeps its record sets but stops following its sources.
